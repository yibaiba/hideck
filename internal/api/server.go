package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/balance"
	"github.com/yibaiba/hideck/internal/commandcenter"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/data/repo"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/global"
	"github.com/yibaiba/hideck/internal/localtls"
	"github.com/yibaiba/hideck/internal/notify"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/internal/proxy/server"
	proxytraffic "github.com/yibaiba/hideck/internal/proxy/traffic"
	"github.com/yibaiba/hideck/internal/updater"
	"github.com/yibaiba/hideck/internal/volte"
	vwebsheet "github.com/yibaiba/hideck/internal/websheet"
	"github.com/yibaiba/hideck/pkg/smscodec"

	"github.com/spf13/viper"
	"github.com/yibaiba/hideck/pkg/logger"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type SMSWithDevice struct {
	db.SMS
	DeviceName string `json:"device_name"`
}

type smsInboxIMSIReader interface {
	GetCachedIMSI() string
	GetIMSI() string
}

func smsInboxIMSI(source smsInboxIMSIReader, liveRefresh bool) string {
	if source == nil {
		return ""
	}
	imsi := strings.TrimSpace(source.GetCachedIMSI())
	if imsi != "" || !liveRefresh {
		return imsi
	}
	return strings.TrimSpace(source.GetIMSI())
}

type loginAttempt struct {
	Count   int
	ResetAt time.Time
}

const updateCheckTimeout = 10 * time.Second

// Server 是 API 服务器的核心结构
type Server struct {
	cfg                     config.ServerConfig // HTTP 服务器配置
	fullCfg                 *config.Config      // 完整配置引用
	pool                    *device.Pool        // 设备工作器池
	utClient                utClientFunc
	auth                    config.WebConfig // Web 认证配置
	fs                      http.FileSystem  // 静态文件系统
	configPath              string           // 配置文件路径
	proxyMgr                *server.Manager  // 代理实例管理器
	trafficRT               realtimeTrafficSubscriber
	proxyRepo               repo.ProxyInstanceRepository
	proxySyncMu             sync.Mutex
	notificationConfigMu    sync.Mutex
	weixinQRMu              sync.Mutex
	wecomQRMu               sync.Mutex
	qqQRMu                  sync.Mutex
	feishuQRMu              sync.Mutex
	voiceGW                 *voicehost.Gateway
	voiceRecordingDirectory string
	phone                   *phone.Service
	phoneCACertificate      string
	notifyMgr               *notify.Manager
	weixinQR                *notify.WeixinQRService
	wecomQR                 *notify.WeComQRService
	qqQR                    *notify.QQQRService
	feishuQR                *notify.FeishuQRService
	commandCenter           *commandcenter.Service
	automaticTasks          automaticTaskService
	balance                 *balance.Service
	carrierRules            carrierRuleStore
	websheets               *vwebsheet.Broker
	cardPolicies            cardPolicyStore
	cardPolicyRestarter     cardPolicyVoWiFiRestarter
	disclaimerAcceptances   disclaimerAcceptanceStore
	systemTime              systemTimeProvider
	backendSwitch           deviceBackendSwitcher
	updateChecker           systemUpdateChecker

	httpSrvMu sync.Mutex
	httpSrv   *http.Server
	httpsSrv  *http.Server

	loginMu                sync.Mutex
	loginAttempts          map[string]loginAttempt
	authMu                 sync.RWMutex
	authChangeMu           sync.Mutex
	passwordChangeRequired bool

	smsLimiterMu sync.Mutex
	smsLimiter   *smsRateLimiter

	shutdownCh       chan struct{}
	backgroundCancel context.CancelFunc
}

type realtimeTrafficSubscriber interface {
	Subscribe(ctx context.Context, deviceID string) (<-chan proxytraffic.RealtimeSnapshot, func())
}

// New 创建一个新的 API 服务器实例
// proxyMgr 参数可为 nil，此时代理管理功能不可用
func New(cfg *config.Config, pool *device.Pool, fs http.FileSystem, proxyMgr *server.Manager, voiceGW *voicehost.Gateway, notifyMgr *notify.Manager, configPath string) *Server {
	if !cfg.Server.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	if strings.TrimSpace(configPath) == "" {
		configPath = "config/config.yaml"
	}
	s := &Server{
		cfg:                   cfg.Server,
		fullCfg:               cfg,
		auth:                  cfg.Web,
		pool:                  pool,
		fs:                    fs,
		configPath:            configPath,
		proxyMgr:              proxyMgr,
		voiceGW:               voiceGW,
		notifyMgr:             notifyMgr,
		weixinQR:              notify.NewWeixinQRService(notify.WeixinQROptions{}),
		wecomQR:               notify.NewWeComQRService(notify.WeComQROptions{}),
		qqQR:                  notify.NewQQQRService(notify.QQQROptions{}),
		feishuQR:              notify.NewFeishuQRService(notify.FeishuQROptions{}),
		proxyRepo:             repo.NewDBRepo(),
		cardPolicies:          databaseCardPolicyStore{},
		disclaimerAcceptances: db.NewDisclaimerAcceptanceStore(db.DB),
		systemTime:            newOSSystemTimeProvider(),
		websheets:             vwebsheet.New(vwebsheet.Config{BasePath: "/api/websheets"}),
		updateChecker: updater.NewChecker(
			&http.Client{Timeout: updateCheckTimeout},
			updater.CheckerOptions{
				CurrentVersion: global.Version,
				IsDocker:       updater.DetectDockerEnvironment(),
			},
		),
		loginAttempts: make(map[string]loginAttempt),
		smsLimiter: newSMSRateLimiterWithConfig(
			time.Now(), time.Now, cfg.Server.SMSRateLimitDisabled,
		),
		shutdownCh: make(chan struct{}),
	}
	s.backendSwitch = newDeviceBackendSwitchService(pool, configPath)
	s.initializeCommandCenter()
	s.initializeAutomaticTasks()
	if cfg.Web.PasswordSource == config.WebPasswordSourceEnvironment && s.passwordStatus("").ChangeRequired {
		logger.Warn("Web 登录密码由环境变量注入且强度不足，请修改后重启服务", "environment_variable", config.WebPasswordEnvironmentVariable)
	}

	return s
}

func (s *Server) initializeCommandCenter() {
	if db.DB == nil || s.pool == nil || s.notifyMgr == nil {
		return
	}
	rules := balance.NewRules(balance.DBCustomRuleSource{})
	s.balance = balance.NewService(balance.NewPoolGateway(s.pool), balance.NewDatabaseStore(db.DB), rules)
	s.carrierRules = databaseCarrierRuleStore{}
	s.commandCenter = commandcenter.NewService(s.notifyMgr.CommandService(), commandcenter.NewDatabaseStore(db.DB))
	_ = s.notifyMgr.SetBalanceCommandHandler(s.handleBalanceCommand)
	s.notifyMgr.SetChannelCommandExecutor(s.commandCenter)
	s.pool.OnInboundSMS(func(message device.InboundSMS) error {
		_, err := s.balance.HandleInboundSMS(context.Background(), balance.InboundSMS{
			DeviceID: message.DeviceID, ICCID: message.ICCID, Sender: message.Sender,
			Content: message.Content, Time: message.Time,
		})
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.backgroundCancel = cancel
	go func() {
		if err := s.balance.RunExpiry(ctx); err != nil {
			logger.Error("余额查询超时任务退出", "err", err)
		}
	}()
}

func (s *Server) SetRealtimeTraffic(m *proxytraffic.RealtimeManager) {
	s.trafficRT = m
}

// SetVoiceRecordingDirectory injects the directory owned by the voice gateway.
func (s *Server) SetVoiceRecordingDirectory(directory string) {
	s.voiceRecordingDirectory = strings.TrimSpace(directory)
}

func (s *Server) SetPhoneService(service *phone.Service) {
	s.phone = service
}

// smsRateLimiter returns the SMS rate limiter, lazily creating it if needed.
func (s *Server) smsRateLimiter() *smsRateLimiter {
	if s.smsLimiter != nil {
		return s.smsLimiter
	}
	s.smsLimiterMu.Lock()
	defer s.smsLimiterMu.Unlock()
	if s.smsLimiter == nil {
		s.smsLimiter = newSMSRateLimiterWithConfig(
			time.Now(), time.Now, s.cfg.SMSRateLimitDisabled,
		)
	}
	return s.smsLimiter
}

// checkPassword 验证密码，支持 bcrypt 哈希和明文（向后兼容）
// stored 是存储的密码（可能是哈希或明文），input 是用户输入的明文密码
func checkPassword(stored, input string) bool {
	if isBcryptPassword(stored) {
		err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(input))
		return err == nil
	}
	// 向后兼容：明文密码对比（常量时间，避免逐字节时序侧信道）
	return subtle.ConstantTimeCompare([]byte(stored), []byte(input)) == 1
}

func (s *Server) issueSessionToken() (string, time.Time, error) {
	exp := time.Now().Add(30 * 24 * time.Hour) // 有效期 30 天
	expStr := strconv.FormatInt(exp.Unix(), 10)

	h := hmac.New(sha256.New, []byte(s.authSnapshot().Password))
	h.Write([]byte(expStr))
	sig := hex.EncodeToString(h.Sum(nil))

	tokenRaw := expStr + "." + sig
	token := base64.StdEncoding.EncodeToString([]byte(tokenRaw))

	return token, exp, nil
}

// pruneExpiredSessionsLocked is removed.

func (s *Server) allowLoginAttempt(ip string, now time.Time) bool {
	if ip == "" {
		ip = "unknown"
	}
	window := 2 * time.Minute
	limit := 10

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	cur := s.loginAttempts[ip]
	if cur.ResetAt.IsZero() || now.After(cur.ResetAt) {
		cur = loginAttempt{Count: 0, ResetAt: now.Add(window)}
	}
	if cur.Count >= limit {
		s.loginAttempts[ip] = cur
		return false
	}
	cur.Count++
	s.loginAttempts[ip] = cur
	return true
}

func (s *Server) newRouter() *gin.Engine {
	r := gin.Default()
	// 不信任任何反向代理传来的 X-Forwarded-For / X-Real-IP，
	// 否则 c.ClientIP()（登录限流、审计日志都依赖它）可被客户端任意伪造。
	// 如果部署在反代之后，应在此显式列出反代的真实 IP/CIDR，而不是信任所有来源。
	if err := r.SetTrustedProxies(nil); err != nil {
		logger.Error("设置 Gin 信任代理列表失败", "err", err)
	}
	r.Use(s.requestIDMiddleware())

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	if s.cfg.Debug {
		r.GET("/debug/embed", s.authMiddleware(), func(c *gin.Context) {
			if s.fs == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"status":  "error",
					"message": "静态资源未启用",
				})
				return
			}

			testFiles := []string{"index.html", "assets", "vite.svg"}
			results := make(map[string]string)
			for _, name := range testFiles {
				f, err := s.fs.Open(name)
				if err != nil {
					results[name] = "ERROR: " + err.Error()
				} else {
					stat, _ := f.Stat()
					if stat.IsDir() {
						results[name] = "DIR"
					} else {
						results[name] = fmt.Sprintf("FILE (size=%d)", stat.Size())
					}
					f.Close()
				}
			}
			c.JSON(http.StatusOK, results)
		})
	}

	// 静态文件服务 (SPA)
	r.NoRoute(s.handleStatic)

	// API 路由组
	api := r.Group("/api")

	// 登录接口 (无需鉴权)
	api.GET("/docs", s.handleAPIDocs)
	api.GET("/docs/assets/*filepath", s.handleDocsAsset)
	api.POST("/auth/login", s.handleLogin)
	api.GET("/phone/ca.crt", s.handlePhoneCACertificate)
	api.POST("/rotateip", s.handleRotate)
	api.OPTIONS("/logs/stream", s.handleLogStreamOptions)
	s.registerWebsheetRoutes(api)

	// 以下接口需要鉴权
	api.Use(s.authMiddleware())
	{
		api.POST("/system/uninstall", s.handleUninstall) // 必须登录后才能调用，见 handleUninstall 内的鉴权检查
		api.GET("/openapi.yaml", s.handleOpenAPIYAML)
		api.GET("/openapi.json", s.handleOpenAPIJSON)

		// ===== 仪表盘 =====
		api.GET("/dashboard/devices", s.handleListDevices)          // 获取所有设备概览（仪表盘卡片用）
		api.GET("/devices/:device_id/status", s.handleStatusDetail) // 获取单个设备详细状态
		api.GET("/health", s.handleHealth)                          // 健康检查（外部监控用）
		api.GET("/traffic/analysis", s.handleTrafficAnalysis)       // 流量分析统计
		s.registerPhoneRoutes(api)
		s.registerUtRoutes(api)

		// ===== 短信 =====
		api.POST("/sms/send", s.handleSendSMS)                    // 发送短信（自动选择 AT 或 VoWiFi）
		api.GET("/sms/delivery/:message_id", s.handleSMSDelivery) // 查询发送投递状态
		api.GET("/sms/contacts", s.handleGetSMSContacts)          // 获取短信联系人列表
		api.GET("/sms/notifications", s.handleSMSNotifications)   // 全局未读数量与增量来信提醒
		api.GET("/sms/messages/:id", s.handleSMSMessageTarget)    // 按消息定位会话与历史窗口
		api.GET("/sms/thread", s.handleGetSMSThread)              // 获取与某联系人的短信会话
		api.PATCH("/sms/thread", s.handleMarkSMSThreadRead)       // 持久化指定会话已读进度
		api.DELETE("/sms/messages/:id", s.handleDeleteSMSMessage) // 删除单条历史短信
		api.DELETE("/sms/thread", s.handleDeleteSMSThread)        // 删除指定历史短信会话

		// ===== 系统设置 =====
		api.GET("/settings/notifications", s.handleGetNotificationSettings)    // 获取通知设置
		api.PUT("/settings/notifications", s.handleUpdateNotificationSettings) // 更新通知设置
		api.GET("/settings/system", s.handleGetSystemSettings)
		api.PUT("/settings/system", s.handleUpdateSystemSettings)
		api.GET("/settings/disclaimer", s.handleGetDisclaimerStatus)
		api.PUT("/settings/disclaimer", s.handleAcceptDisclaimer)
		api.GET("/settings/password", s.handleGetPasswordStatus)
		api.POST("/settings/notifications/webhook/test", s.handleTestWebhookNotification)
		api.POST("/settings/notifications/bark/test", s.handleTestBarkNotification)
		api.POST("/settings/notifications/email/test", s.handleTestEmailNotification)
		api.POST("/settings/notifications/wecom/test", s.handleTestWeComNotification)
		api.POST("/settings/notifications/weixin/qr/start", s.handleStartWeixinQR)
		api.GET("/settings/notifications/weixin/qr/status", s.handleWeixinQRStatus)
		api.POST("/settings/notifications/weixin/qr/cancel", s.handleCancelWeixinQR)
		api.POST("/settings/notifications/wecom-bot/qr/start", s.handleStartWeComQR)
		api.GET("/settings/notifications/wecom-bot/qr/status", s.handleWeComQRStatus)
		api.POST("/settings/notifications/wecom-bot/qr/cancel", s.handleCancelWeComQR)
		api.POST("/settings/notifications/qq/qr/start", s.handleStartQQQR)
		api.GET("/settings/notifications/qq/qr/status", s.handleQQQRStatus)
		api.POST("/settings/notifications/qq/qr/cancel", s.handleCancelQQQR)
		api.POST("/settings/notifications/feishu/qr/start", s.handleStartFeishuQR)
		api.GET("/settings/notifications/feishu/qr/status", s.handleFeishuQRStatus)
		api.POST("/settings/notifications/feishu/qr/cancel", s.handleCancelFeishuQR)
		api.POST("/settings/password", s.handleChangePassword) // 修改登录密码
		api.GET("/system/info", s.handleSystemInfo)            // 获取系统运行与版本信息
		api.GET("/system/time", s.handleSystemTime)            // 获取设备时间和时区
		api.GET("/system/update/check", s.handleCheckUpdate)   // 检查系统更新
		api.POST("/system/update/apply", s.handleApplyUpdate)  // 应用系统更新

		api.GET("/devices", s.handleDeviceMgmtList)                                            // 获取设备列表（管理页用）
		api.POST("/devices", s.handleDeviceMgmtAddDevice)                                      // 添加新设备
		api.GET("/devices/discovered", s.handleDeviceMgmtDiscovered)                           // 获取已发现的硬件设备
		api.POST("/devices/actions/rescan", s.handleDeviceRescan)                              // 手动触发设备重扫描
		api.GET("/devices/:device_id/overview/stream", s.handleDeviceMgmtOverviewStreamSingle) // SSE 单体深层实时流
		api.GET("/devices/:device_id/overview", s.handleDeviceMgmtOverviewLite)                // 获取设备详情（轻量版）
		api.GET("/devices/:device_id/config", s.handleDeviceMgmtGetDeviceConfig)               // 获取设备配置
		api.PUT("/devices/:device_id", s.handleDeviceMgmtUpdateDevice)                         // 更新设备配置
		api.DELETE("/devices/:device_id", s.handleDeviceMgmtDeleteDevice)                      // 删除设备
		api.POST("/devices/:device_id/actions/refresh", s.handleDeviceMgmtRefreshInfo)         // 手动触发刷新设备缓存信息
		api.POST("/devices/:device_id/actions/retry-sim-pin", s.handleDeviceMgmtRetryPCSCPIN)  // 允许当前 PC/SC SIM 重新验证 PIN
		api.POST("/devices/:device_id/actions/reboot", s.handleDeviceMgmtReboot)               // 重启设备模组
		api.POST("/devices/:device_id/actions/at", s.handleDeviceMgmtExecuteAT)                // 执行 AT 命令
		api.POST("/devices/:device_id/actions/ussd", s.handleDeviceMgmtExecuteUSSD)            // 执行 USSD 指令
		api.POST("/devices/:device_id/actions/ussd/continue", s.handleDeviceMgmtContinueUSSD)  // USSD 续轮输入（多轮交互）
		api.POST("/devices/:device_id/actions/ussd/cancel", s.handleDeviceMgmtCancelUSSD)      // 取消 USSD 会话
		api.POST("/devices/:device_id/vowifi/calls", s.handleDeviceVoWiFiCall)                 // 发起真实 VoWiFi 外呼
		api.PATCH("/devices/:device_id/usbnet-mode", s.handleDeviceMgmtSetUSBNetMode)          // 设置 USBNET 模式
		api.PATCH("/devices/:device_id/flight-mode", s.handleDeviceMgmtSetFlightMode)          // 切换飞行模式
		api.PATCH("/devices/:device_id/network", s.handleDeviceNetworkPatch)

		api.GET("/cards/policies", s.handleListCardPolicies)
		api.GET("/cards/:iccid/policy", s.handleGetCardPolicy)
		api.PUT("/cards/:iccid/policy", s.handlePutCardPolicy)

		api.GET("/devices/:device_id/operator_selection/scan", s.handleDeviceMgmtOperatorScan)              // 扫描运营商
		api.GET("/devices/:device_id/operator_selection/scan/stream", s.handleDeviceMgmtOperatorScanStream) // SSE 扫描运营商
		api.GET("/devices/:device_id/operator_selection", s.handleDeviceMgmtGetOperatorSelection)           // 获取当前选网配置
		api.POST("/devices/:device_id/operator_selection", s.handleDeviceMgmtSetOperatorSelection)          // 锁定运营商或恢复自动

		// ===== 代理管理 =====
		api.GET("/proxy-instances/overview", s.handleProxyOverview)                             // 获取代理实例概览
		api.PUT("/proxy-instances/config", s.handleProxyUpdateConfig)                           // 保存代理配置
		api.GET("/proxy-instances/:instance_id", s.handleProxyInstanceGet)                      // 获取单个代理实例
		api.POST("/proxy-instances/:instance_id/actions/start", s.handleProxyInstanceStart)     // 启动代理实例
		api.POST("/proxy-instances/:instance_id/actions/stop", s.handleProxyInstanceStop)       // 停止代理实例
		api.POST("/proxy-instances/:instance_id/actions/restart", s.handleProxyInstanceRestart) // 重启代理实例

		// ===== 前置代理管理 =====
		api.GET("/upstream-proxies", s.handleListUpstreamProxies)                                         // 列出所有前置代理
		api.POST("/upstream-proxies", s.handleCreateUpstreamProxy)                                        // 新增前置代理
		api.PUT("/upstream-proxies/:proxy_id", s.handleUpdateUpstreamProxy)                               // 更新前置代理
		api.DELETE("/upstream-proxies/:proxy_id", s.handleDeleteUpstreamProxy)                            // 删除前置代理
		api.POST("/upstream-proxies/:proxy_id/actions/probe", s.handleProbeUpstreamProxy)                 // 探测前置代理
		api.GET("/upstream-proxy-countries", s.handleListUpstreamProxyCountries)                          // 列出可配置国家
		api.GET("/upstream-proxy-country-rules", s.handleListUpstreamProxyCountryRules)                   // 列出国家规则
		api.PUT("/upstream-proxy-country-rules/:country_code", s.handleUpsertUpstreamProxyCountryRule)    // 保存国家规则
		api.DELETE("/upstream-proxy-country-rules/:country_code", s.handleDeleteUpstreamProxyCountryRule) // 删除国家规则

		// ===== eSIM =====
		api.GET("/devices/:device_id/esim", s.handleEsimGetOverview) // 获取 eSIM 总览
		api.GET("/devices/:device_id/esim/profiles", s.handleEsimListProfiles)
		api.GET("/devices/:device_id/esim/notifications", s.handleEsimListNotifications)
		api.POST("/devices/:device_id/esim/actions/disable", s.handleEsimDisableProfile)                          // 停用 eSIM profile
		api.POST("/devices/:device_id/esim/notifications/:sequence/actions/retry", s.handleEsimRetryNotification) // 获取 eSIM profile 列表
		api.POST("/devices/:device_id/esim/actions/switch", s.handleEsimSwitchProfile)                            // 切换 eSIM profile
		api.GET("/devices/:device_id/esim/eids", s.handleEsimGetEID)                                              // 获取 EID
		api.GET("/devices/:device_id/esim/chip-info", s.handleEsimGetChipInfo)                                    // 获取 eUICC 芯片信息
		api.GET("/devices/:device_id/esim/actions/download", s.handleEsimDownloadProfile)                         // 下载 eSIM profile（SSE 流式进度）
		api.POST("/devices/:device_id/esim/actions/decode-activation", s.handleEsimDecodeActivation)              // 从图片或 PDF 识别激活码
		api.PATCH("/devices/:device_id/esim/profiles/:iccid", s.handleEsimRenameProfile)                          // 修改 profile 名称
		api.DELETE("/devices/:device_id/esim/profiles/:iccid", s.handleEsimDeleteProfile)                         // 删除 eSIM profile
		api.POST("/devices/:device_id/esim/profiles/:iccid/actions/recover-lebara-identity", s.handleEsimRecoverLebaraIdentity)

		// ===== VoWiFi =====
		api.PATCH("/devices/:device_id/vowifi", s.handleDeviceVoWiFiPatch)                          // 启用/禁用 VoWiFi
		api.POST("/devices/:device_id/vowifi/actions/reconnect", s.handleDeviceMgmtReconnectVoWiFi) // 重连 VoWiFi
		api.POST("/devices/:device_id/vowifi/e911/websheet", s.handleDeviceE911Websheet)            // 打开 E911 设置 websheet

		// ===== 日志 =====
		api.GET("/logs/stream", s.handleLogStream)   // SSE 实时日志流
		api.GET("/logs/history", s.handleLogHistory) // 获取历史日志

		// ===== 命令中心与余额查询 =====
		s.registerCommandCenterRoutes(api)
		s.registerAutomaticTaskRoutes(api)
	}
	return r
}

func (s *Server) Run() error {
	if !s.cfg.HTTPSEnabled {
		s.phoneCACertificate = ""
		return s.serveHTTP()
	}
	tlsFiles, err := localtls.Ensure(localtls.Config{
		CertificateFile: s.cfg.TLSCertFile, PrivateKeyFile: s.cfg.TLSKeyFile,
		DataDirectory: s.cfg.TLSDataDir,
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTPS 证书: %w", err)
	}
	certificate, err := tls.LoadX509KeyPair(tlsFiles.CertificateFile, tlsFiles.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("加载 HTTPS 证书: %w", err)
	}
	s.phoneCACertificate = tlsFiles.CACertificateFile
	return s.serveHTTPAndHTTPS(certificate)
}

func withCommandEventStreamDeadlineDisabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isCommandEventStreamPath(request.URL.Path) {
			controller := http.NewResponseController(writer)
			if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				logger.Warn("取消命令中心实时流写入超时失败", "err", err)
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func isCommandEventStreamPath(path string) bool {
	return path == "/api/command-center/stream" || path == "/api/commands/events/stream" || path == "/api/phone/events"
}

func (s *Server) Shutdown(ctx context.Context) error {
	var automaticTaskErr error
	if s.automaticTasks != nil {
		automaticTaskErr = s.automaticTasks.Stop(ctx)
	}
	if s.backgroundCancel != nil {
		s.backgroundCancel()
	}
	var phoneErr error
	if s.phone != nil {
		phoneErr = s.phone.Close(ctx)
	}
	// 广播关闭信号给所有内部持有的长连接（如 SSE），让它们主动退出
	select {
	case <-s.shutdownCh:
	default:
		close(s.shutdownCh)
	}

	s.httpSrvMu.Lock()
	httpServer, httpsServer := s.httpSrv, s.httpsSrv
	s.httpSrvMu.Unlock()
	if httpServer == nil && httpsServer == nil {
		return errors.Join(automaticTaskErr, phoneErr)
	}
	shutdownErrors := make(chan error, 2)
	shutdownCount := 0
	if httpServer != nil {
		shutdownCount++
		go func() { shutdownErrors <- httpServer.Shutdown(ctx) }()
	}
	if httpsServer != nil {
		shutdownCount++
		go func() { shutdownErrors <- httpsServer.Shutdown(ctx) }()
	}
	result := errors.Join(automaticTaskErr, phoneErr)
	for range shutdownCount {
		result = errors.Join(result, <-shutdownErrors)
	}
	return result
}

func (s *Server) requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader("X-Request-Id"))
		if requestID == "" {
			b := make([]byte, 8)
			if _, err := rand.Read(b); err == nil {
				requestID = hex.EncodeToString(b)
			} else {
				requestID = fmt.Sprintf("%d", time.Now().UnixNano())
			}
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-Id", requestID)
		c.Next()
	}
}

func requestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get("request_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (s *Server) handleListDevices(c *gin.Context) {
	workers := s.pool.GetAllWorkers()
	cfgByID := map[string]config.DeviceConfig{}
	{
		managed := config.ListDevices()
		for _, d := range managed {
			cfgByID[d.ID] = d
		}
	}

	type DeviceStatus struct {
		ID               string                            `json:"id"`
		Name             string                            `json:"name"`
		Interface        string                            `json:"interface"`
		ProxyPort        int                               `json:"proxy_port"`
		PublicIP         string                            `json:"public_ip"`
		PublicIPv6       string                            `json:"public_ipv6,omitempty"`
		Healthy          bool                              `json:"healthy"`
		Operator         string                            `json:"operator"`
		SignalDBM        int                               `json:"signal_dbm"`
		NetworkMode      string                            `json:"network_mode"`
		NetworkDuplex    string                            `json:"network_duplex"`
		PhoneMode        string                            `json:"phone_mode,omitempty"`
		VoWiFiActive     bool                              `json:"vowifi_active"`
		VoWiFiRuntime    *voWiFiRuntimeDTO                 `json:"vowifi_runtime,omitempty"`
		VoWiFiHealth     *device.WiFiCallingHealthSnapshot `json:"vowifi_health,omitempty"`
		NativeVoLTE      *volte.Status                     `json:"native_volte,omitempty"`
		Traffic          map[string]string                 `json:"traffic,omitempty"`
		NetworkConnected bool                              `json:"network_connected"`
	}

	list := make([]DeviceStatus, 0, len(workers))
	for _, w := range workers {
		status := w.GetCachedDeviceStatus() // 仓表盘列表读缓存，0 IPC
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			// PhoneMode 等策略字段只在 worker 运行时投影，yaml 里是空的。
			// 直接用 persisted 会把联通原生 VoLTE 画成 VoWiFi。
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		item := DeviceStatus{
			ID:               cfg.ID,
			Name:             cfg.Name,
			Interface:        cfg.Interface,
			ProxyPort:        cfg.ProxyPort,
			PublicIP:         w.GetCachedIP(),
			PublicIPv6:       w.GetCachedIPv6(),
			Healthy:          w.GetCachedHealthy(), // 健康状态读缓存
			Operator:         device.DisplayOperatorName(status, s.pool.IsVoWiFiActive(w.ID)),
			SignalDBM:        status.SignalDBM,
			NetworkMode:      status.NetworkMode,
			NetworkDuplex:    status.NetworkDuplex,
			PhoneMode:        overviewPhoneMode(cfg.PhoneMode),
			VoWiFiActive:     s.pool.IsVoWiFiActive(w.ID), // 逐个设备判断 VoWiFi 状态，支持多设备
			VoWiFiRuntime:    s.getVoWiFiRuntimeDTO(w.ID),
			VoWiFiHealth:     s.getWiFiCallingHealth(w.ID),
			NetworkConnected: w.NetworkConnected(),
		}
		if device.IsNativeVoLTEMode(cfg.PhoneMode) {
			volteStatus := s.pool.NativeVoLTEStatus(w.ID)
			item.NativeVoLTE = &volteStatus
		}
		// 添加格式化流量
		if w.Proxy != nil {
			item.Traffic = w.Proxy.GetFormattedStats()
		}
		list = append(list, item)
	}
	c.JSON(http.StatusOK, list)
}

// handleDeviceRescan 手动触发设备重新扫描
func (s *Server) handleDeviceRescan(c *gin.Context) {
	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}

	if err := s.pool.RescanAndReconnect(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "重新扫描失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": "设备重新扫描完成",
	})
}

// handleLogStream SSE 实时日志流
func (s *Server) handleLogStream(c *gin.Context) {
	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	s.setLogStreamCORSHeaders(c)

	// 订阅日志流
	logChan := logger.GlobalBroadcaster.Subscribe()
	defer logger.GlobalBroadcaster.Unsubscribe(logChan)

	// 获取日志级别过滤参数
	levelFilter := c.Query("level") // debug, info, warn, error

	// 客户端连接上下文
	clientGone := c.Request.Context().Done()

	// 发送初始连接成功事件
	c.SSEvent("connected", gin.H{"message": "已连接日志流"})
	c.Writer.Flush()

	for {
		select {
		case <-clientGone:
			return
		case <-s.shutdownCh:
			return
		case entry, ok := <-logChan:
			if !ok {
				return
			}

			// 日志级别过滤
			if levelFilter != "" {
				entryLevel := strings.ToLower(entry.Level)
				filterLevel := strings.ToLower(levelFilter)
				if !matchLogLevel(entryLevel, filterLevel) {
					continue
				}
			}

			// 发送日志条目
			c.SSEvent("log", entry)
			c.Writer.Flush()
		}
	}
}

func (s *Server) handleLogStreamOptions(c *gin.Context) {
	s.setLogStreamCORSHeaders(c)
	c.Status(http.StatusNoContent)
}

func (s *Server) setLogStreamCORSHeaders(c *gin.Context) {
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" {
		return
	}

	if s.isAllowedLogStreamOrigin(origin, c.Request.Host) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Max-Age", "600")
		c.Header("Vary", "Origin")
	}
}

func (s *Server) isAllowedLogStreamOrigin(origin, host string) bool {
	if origin == "" {
		return false
	}

	host = strings.TrimSpace(host)
	if host != "" {
		if origin == "http://"+host || origin == "https://"+host {
			return true
		}
	}

	if !s.cfg.Debug {
		return false
	}

	if strings.HasPrefix(origin, "http://localhost:") || origin == "http://localhost" {
		return true
	}
	if strings.HasPrefix(origin, "http://127.0.0.1:") || origin == "http://127.0.0.1" {
		return true
	}
	if strings.HasPrefix(origin, "http://[::1]:") || origin == "http://[::1]" {
		return true
	}
	return false
}

// matchLogLevel 判断日志级别是否符合过滤条件
// filterLevel: debug 显示所有，info 显示 info/warn/error，warn 显示 warn/error，error 只显示 error
func matchLogLevel(entryLevel, filterLevel string) bool {
	levels := map[string]int{
		"debug": 0,
		"info":  1,
		"warn":  2,
		"error": 3,
		"fatal": 4,
	}
	entryLvl, entryOk := levels[entryLevel]
	filterLvl, filterOk := levels[filterLevel]
	if !entryOk || !filterOk {
		return true
	}
	return entryLvl >= filterLvl
}

// handleLogHistory 获取历史日志
func (s *Server) handleLogHistory(c *gin.Context) {
	// 读取参数
	lines := 500 // 默认返回最近 500 行
	if n := c.Query("lines"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 && v <= 2000 {
			lines = v
		}
	}

	// 日志文件路径（使用 logger 的默认路径）
	logFile := "logs/app.log"

	recentLines, err := readLastLines(logFile, lines, 1<<20)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"logs": []logger.LogEntry{}, "error": "无法读取日志文件"})
		return
	}

	// 解析日志行
	entries := make([]logger.LogEntry, 0, len(recentLines))
	for _, line := range recentLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry := parseLogLine(line)
		if entry.Message != "" {
			entries = append(entries, entry)
		}
	}

	c.JSON(http.StatusOK, gin.H{"logs": entries})
}

func readLastLines(path string, maxLines int, maxBytes int64) ([]string, error) {
	if maxLines <= 0 {
		return []string{}, nil
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 {
		return []string{}, nil
	}

	const blockSize int64 = 32 * 1024
	var offset = size
	var readBytes int64
	var newlines int
	chunks := make([][]byte, 0, 8)

	for offset > 0 && readBytes < maxBytes && newlines <= maxLines {
		step := blockSize
		if offset < step {
			step = offset
		}
		offset -= step

		b := make([]byte, step)
		n, rerr := f.ReadAt(b, offset)
		if rerr != nil && n == 0 {
			return nil, rerr
		}
		b = b[:n]

		chunks = append(chunks, b)
		readBytes += int64(n)
		newlines += bytes.Count(b, []byte{'\n'})
	}

	var buf bytes.Buffer
	for i := len(chunks) - 1; i >= 0; i-- {
		buf.Write(chunks[i])
	}

	all := strings.Split(buf.String(), "\n")
	for len(all) > 0 && strings.TrimSpace(all[len(all)-1]) == "" {
		all = all[:len(all)-1]
	}
	if len(all) <= maxLines {
		return all, nil
	}
	return all[len(all)-maxLines:], nil
}

// parseLogLine 解析日志行
// 格式: [2026-02-06 15:04:05] LEVEL  caller  message {fields}
func parseLogLine(line string) logger.LogEntry {
	entry := logger.LogEntry{}

	// 提取时间 [2026-02-06 15:04:05]
	if len(line) < 22 || line[0] != '[' {
		entry.Message = line
		return entry
	}

	timeEnd := strings.Index(line, "]")
	if timeEnd < 0 {
		entry.Message = line
		return entry
	}

	timeStr := line[1:timeEnd]
	// 使用 ParseInLocation 确保日志时间继承系统本地时区，避免被默认解析为 UTC
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.Local); err == nil {
		entry.Time = t.Format(time.RFC3339)
	} else {
		entry.Time = timeStr
	}

	rest := strings.TrimSpace(line[timeEnd+1:])

	// 使用 Fields 按任意空白字符分割
	fields := strings.Fields(rest)
	if len(fields) >= 1 {
		entry.Level = fields[0]
	}
	if len(fields) >= 2 {
		entry.Caller = fields[1]
	}
	if len(fields) >= 3 {
		// message 是剩余部分，需要从原字符串中找到正确位置
		// 找到 caller 在 rest 中的位置
		callerIdx := strings.Index(rest, entry.Caller)
		if callerIdx >= 0 {
			msgStart := callerIdx + len(entry.Caller)
			entry.Message = strings.TrimSpace(rest[msgStart:])
		} else {
			entry.Message = strings.Join(fields[2:], " ")
		}
	}

	return entry
}

func (s *Server) handleRotate(c *gin.Context) {
	var req struct {
		DeviceID       string `json:"device_id" form:"device_id"`
		Username       string `json:"username" form:"username"`
		Password       string `json:"password" form:"password"`
		LegacyDeviceID string `json:"device" form:"device"`
	}
	_ = c.ShouldBind(&req)

	if !s.authorizeRotate(c, req.Username, req.Password, time.Now()) {
		return
	}

	deviceID := c.Query("device_id")
	if deviceID == "" {
		if req.DeviceID != "" {
			deviceID = req.DeviceID
		} else if req.LegacyDeviceID != "" {
			deviceID = req.LegacyDeviceID
		}
	}

	// 如果未指定设备 ID 且只有一个设备，默认使用该设备
	if deviceID == "" {
		workers := s.pool.GetAllWorkers()
		if len(workers) == 1 {
			deviceID = workers[0].ID
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "存在多个设备时必须指定 device_id"})
			return
		}
	}

	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}
	nc := worker.NetworkController()
	if nc == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "当前设备不支持网络控制",
		})
		return
	}
	if !worker.NetworkConnected() {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "设备网络未连接，请先启动网络",
		})
		return
	}

	// 执行切换 (同步操作，带重试和通知)
	startTime := time.Now()
	oldIP, newIP, err := worker.Rotate()
	duration := time.Since(startTime)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":   "error",
			"message":  err.Error(),
			"old_ip":   oldIP,
			"new_ip":   newIP,
			"duration": duration.String(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"message":  "IP 切换成功",
		"device":   deviceID,
		"old_ip":   oldIP,
		"new_ip":   newIP,
		"duration": duration.String(),
	})
}

func (s *Server) handleDeviceMgmtStartNetwork(c *gin.Context) {
	deviceID := deviceIDParam(c)
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}
	nc := worker.NetworkController()
	if nc == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "当前设备不支持网络控制"})
		return
	}
	if s.pool.IsVoWiFiActive(deviceID) && !device.PhoneModeCampsOnCell(worker.Config.PhoneMode) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "VoWiFi 运行中，无法启动数据网络"})
		return
	}
	if err := worker.StartNetwork(); err != nil {
		if writeLebaraUKRFLockError(c, err) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "启动数据网络失败: " + err.Error()})
		return
	}
	go func() { _ = worker.RefreshRuntime(nil, "start_network") }()
	c.JSON(http.StatusOK, gin.H{
		"status":            "ok",
		"message":           "数据网络已启动",
		"device":            deviceID,
		"network_connected": worker.NetworkConnected(),
		"private_ip":        nc.GetPrivateIP(),
		"private_ipv6":      nc.GetPrivateIPv6(),
		"public_ip":         worker.GetCachedIP(),
		"public_ipv6":       worker.GetCachedIPv6(),
	})
}

func (s *Server) handleDeviceMgmtStopNetwork(c *gin.Context) {
	deviceID := deviceIDParam(c)
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}
	nc := worker.NetworkController()
	if nc == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "当前设备不支持网络控制"})
		return
	}
	if err := worker.StopNetwork(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "停止数据网络失败: " + err.Error()})
		return
	}
	go func() { _ = worker.RefreshRuntime(nil, "stop_network") }()
	c.JSON(http.StatusOK, gin.H{
		"status":            "ok",
		"message":           "数据网络已停止",
		"device":            deviceID,
		"network_connected": worker.NetworkConnected(),
		"private_ip":        "",
		"private_ipv6":      "",
		"public_ip":         "",
		"public_ipv6":       "",
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	workers := s.pool.GetAllWorkers()
	allHealthy := true

	type DeviceHealth struct {
		Healthy          bool `json:"healthy"`
		ModemOK          bool `json:"modem_ok"`
		IfaceUp          bool `json:"iface_up"`
		NetworkConnected bool `json:"network_connected"`
		Signal           int  `json:"signal,omitempty"`
	}

	status := make(map[string]DeviceHealth)
	for _, w := range workers {
		modemOK := w.IsDeviceHealthy()
		ifaceUp := false
		healthy := modemOK

		if w.QMICore != nil {
			ifaceUp = w.QMICore.IsInterfaceUp()
		}

		// 获取信号 (非阻塞)
		signal := 0
		stats := w.GetStats()
		if s, ok := stats["signal"].(int); ok {
			signal = s
		}

		status[w.ID] = DeviceHealth{
			Healthy:          healthy,
			ModemOK:          modemOK,
			IfaceUp:          ifaceUp,
			NetworkConnected: ifaceUp,
			Signal:           signal,
		}
		if !healthy {
			allHealthy = false
		}
	}

	if allHealthy {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "devices": status})
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "devices": status})
	}
}

func (s *Server) handleSendSMS(c *gin.Context) {
	type SendSMSRequest struct {
		DeviceID string `json:"device_id"`
		IMSI     string `json:"imsi"`
		ICCID    string `json:"iccid"`
		Phone    string `json:"phone" binding:"required"`
		Message  string `json:"message" binding:"required"`
		Encoding string `json:"encoding"`
	}

	var req SendSMSRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误: " + err.Error()})
		return
	}

	encoding, err := smscodec.NormalizeSMSEncoding(req.Encoding)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "短信编码参数错误: " + err.Error()})
		return
	}
	worker, status, msg := s.resolveSMSSendWorker(req.DeviceID, req.IMSI, req.ICCID)
	if status != 0 {
		c.JSON(status, gin.H{"status": "error", "message": msg})
		return
	}
	deviceID := worker.ID
	sendOpts := smscodec.SubmitOptions{Encoding: encoding}

	if rate := s.smsRateLimiter().Allow(); !rate.Allowed {
		retryAfterSeconds := int64(rate.RetryAfter.Seconds())
		if retryAfterSeconds < 0 {
			retryAfterSeconds = 0
		}
		c.JSON(http.StatusTooManyRequests, gin.H{
			"status":              "error",
			"code":                "sms_rate_limited",
			"reason":              rate.Code,
			"message":             rate.Message,
			"retry_after_seconds": retryAfterSeconds,
			"request_id":          requestID(c),
		})
		return
	}

	// 获取 IMSI 用于入库
	imsi := worker.GetIMSI()
	messageID := ""
	partsTotal := 1
	deliveryState := "acked"

	routed, err := s.pool.SendRoutedSMS(c.Request.Context(), worker, req.Phone, req.Message, sendOpts)
	if routed.Outcome.PartsTotal > 0 {
		partsTotal = routed.Outcome.PartsTotal
	}
	if strings.TrimSpace(routed.Outcome.DeliveryState) != "" {
		deliveryState = strings.TrimSpace(routed.Outcome.DeliveryState)
	}
	messageID = strings.TrimSpace(routed.Outcome.MessageID)
	if err != nil {
		if routed.Via == device.RoutedSMSViaVoWiFi || routed.FellBackToCS {
			deliveryState = "failed"
			_ = device.RecordVoWiFiSMSSendFailure(s.pool, deviceID, req.Phone, req.Message, time.Now())
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":         "error",
				"message":        "VoWiFi 短信发送失败: " + err.Error(),
				"device":         deviceID,
				"phone":          req.Phone,
				"message_id":     messageID,
				"parts_total":    partsTotal,
				"delivery_state": deliveryState,
			})
			return
		}
		if imsi != "" {
			if persistErr := db.SaveSMS(imsi, worker.ID, req.Phone, req.Message, 2, 3, time.Now()); persistErr != nil {
				logger.Warn("短信发送失败记录入库失败", "device", deviceID, "err", persistErr)
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "发送失败: " + err.Error(),
			"device":  deviceID,
			"phone":   req.Phone,
		})
		return
	}
	if routed.Via == device.RoutedSMSViaCS {
		if routed.FellBackToCS {
			deliveryState = "acked"
		}
		if imsi != "" {
			if persistErr := db.SaveSMS(imsi, worker.ID, req.Phone, req.Message, 2, 2, time.Now()); persistErr != nil {
				logger.Warn("短信发送成功但入库失败", "device", deviceID, "err", persistErr)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"message":        "短信发送成功",
		"device":         deviceID,
		"phone":          req.Phone,
		"message_id":     messageID,
		"parts_total":    partsTotal,
		"delivery_state": deliveryState,
	})
}

func (s *Server) handleSMSDelivery(c *gin.Context) {
	messageID := strings.TrimSpace(c.Param("message_id"))
	if messageID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "message_id 不能为空"})
		return
	}
	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}
	services := s.pool.GetAllVoWiFiApps()
	for _, svc := range services {
		if svc == nil {
			continue
		}
		status, err := svc.GetSMSDeliveryStatusContext(c.Request.Context(), messageID)
		if err != nil {
			continue
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "delivery": status})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "未找到对应短信投递记录"})
}

// handleVoWiFiEnable 为指定设备启用 VoWiFi
func (s *Server) handleVoWiFiEnable(c *gin.Context) {
	deviceID := deviceIDParam(c)
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "请指定设备 ID"})
		return
	}

	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}

	if err := s.pool.EnableVoWiFi(deviceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "VoWiFi 启用失败: " + err.Error(),
			"device":  deviceID,
		})
		return
	}

	message := "VoWiFi 已启用，设备已进入飞行模式"
	if worker := s.pool.GetWorker(deviceID); worker != nil && worker.Config.PhoneMode == "cellular" {
		if worker.Config.NetworkEnabled || worker.Config.DataStrategy == "always" {
			message = "蜂窝数据通话已启用"
		} else {
			message = "蜂窝模式已设置，会正常驻网。打开「网络」后才会走流量"
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": message,
		"device":  deviceID,
	})
}

// handleVoWiFiDisable 禁用 VoWiFi，保留当前射频/网络状态
func (s *Server) handleVoWiFiDisable(c *gin.Context) {
	deviceID := deviceIDParam(c)

	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}

	if err := s.pool.DisableVoWiFi(deviceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "VoWiFi 禁用失败: " + err.Error(),
			"device":  deviceID,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": "VoWiFi 已禁用",
		"device":  deviceID,
	})
}

// handleStatusDetail 返回单个设备的详细状态
func (s *Server) handleStatusDetail(c *gin.Context) {
	deviceID := deviceIDParam(c)
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}

	_ = worker.RefreshRuntime(c.Request.Context(), "status_detail")
	_ = worker.RefreshIdentityLive(c.Request.Context(), "status_detail")
	status := worker.ProjectDeviceStatus()

	response := gin.H{
		"id":                worker.ID,
		"name":              worker.Config.Name,
		"imei":              status.IMEI,
		"firmware":          status.Firmware,
		"iccid":             status.ICCID,
		"imsi":              status.IMSI,
		"native_spn":        status.NativeSPN,
		"native_mcc":        status.NativeMCC,
		"native_mnc":        status.NativeMNC,
		"gid1":              status.GID1,
		"gid2":              status.GID2,
		"pnn":               status.PNN,
		"opl":               status.OPL,
		"sim_service_table": status.SIMServiceTable,
		"operator":          status.Operator,
		"sim_inserted":      status.SimInserted,
		"signal_dbm":        status.SignalDBM,
		"signal_rsrp":       status.SignalRSRP,
		"signal_rsrq":       status.SignalRSRQ,
		"signal_sinr":       status.SignalSINR,
		"nr5g_signal_sinr":  status.NR5GSignalSINR,
		"radio_band":        status.RadioBand,
		"radio_channel":     status.RadioChannel,
		"reg_status":        status.RegStatus,
		"reg_status_text":   status.RegStatusText,
		"lac":               status.LAC,
		"cell_id":           status.CellID,
		"apn":               status.APN,
		"ims_status":        status.IMSStatus,
		"public_ip":         worker.GetCachedIP(),
		"public_ipv6":       worker.GetCachedIPv6(),
		"interface":         worker.Config.Interface,
		"proxy_port":        worker.Config.ProxyPort,
		"healthy":           worker.IsDeviceHealthy(),
		"network_connected": worker.NetworkConnected(),
	}

	if worker.Proxy != nil {
		response["traffic"] = worker.Proxy.GetFormattedStats()
	}

	vowifi := gin.H{
		"active": s.pool.IsVoWiFiActive(worker.ID),
	}
	if obs := s.pool.GetVoWiFiObs(worker.ID); obs != nil {
		for k, v := range obs {
			vowifi[k] = v
		}
	} else {
		if app := s.pool.GetVoWiFiAppForDevice(worker.ID); app != nil {
			status := app.Status()
			vowifi["imscore"] = status
			vowifi["smsip"] = status
		}
	}
	if s.voiceGW != nil {
		vowifi["voice"] = s.voiceGW.DeviceStatus(worker.ID)
	}
	response["vowifi"] = vowifi

	c.JSON(http.StatusOK, response)
}

type SMSContactWithDevice struct {
	db.SMSContact
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	LocalPhone string `json:"local_phone"` // 本机号码（收件人手机号），来自订阅手机号
}

func (s *Server) handleGetSMSContacts(c *gin.Context) {
	deviceID := c.Query("device_id")
	imsi := c.Query("imsi")

	limitStr := c.DefaultQuery("limit", "50")
	var limit int
	fmt.Sscanf(limitStr, "%d", &limit)

	var beforeTs *time.Time
	if v := strings.TrimSpace(c.Query("before_ts")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			beforeTs = &t
		}
	}
	beforePeer := strings.TrimSpace(c.Query("before_peer"))

	var iccid string
	if strings.TrimSpace(deviceID) != "" {
		resolved, status, msg := s.resolveSMSICCID(deviceID, imsi)
		if status != 0 {
			c.JSON(status, gin.H{"status": "error", "message": msg})
			return
		}
		iccid = resolved
	}

	var contacts []db.SMSContact
	var err error
	if iccid != "" {
		contacts, err = db.GetSMSContactsByICCID(iccid, limit, beforeTs, beforePeer)
	} else {
		contacts, err = db.GetSMSContacts(limit, beforeTs, beforePeer)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "查询数据库失败: " + err.Error()})
		return
	}

	iccidDevice := s.smsDeviceInfoByICCID()

	// 手机号仍通过 IMSI 从 sim_subscriptions 查询（sim_subscriptions 主键尚为 IMSI）。
	imsiPhone := make(map[string]string)
	if phones, err := db.GetSIMPhoneNumbersByIMSI(); err == nil {
		imsiPhone = phones
	}

	enriched := make([]SMSContactWithDevice, 0, len(contacts))
	for _, ct := range contacts {
		info := iccidDevice[ct.ICCID]
		enriched = append(enriched, SMSContactWithDevice{
			SMSContact: ct,
			DeviceID:   info.id,
			DeviceName: info.name,
			LocalPhone: imsiPhone[ct.IMSI],
		})
	}

	c.JSON(http.StatusOK, enriched)
}

func (s *Server) handleGetSMSThread(c *gin.Context) {
	deviceID := c.Query("device_id")
	imsi := c.Query("imsi")
	requestedICCID := db.CanonicalICCID(c.Query("iccid"))
	peer := strings.TrimSpace(c.Query("peer"))
	if peer == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "缺少 peer 参数"})
		return
	}

	limitStr := c.DefaultQuery("limit", "50")
	var limit int
	fmt.Sscanf(limitStr, "%d", &limit)

	var beforeTs *time.Time
	if v := strings.TrimSpace(c.Query("before_ts")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			beforeTs = &t
		}
	}
	var beforeID uint
	if v := strings.TrimSpace(c.Query("before_id")); v != "" {
		var parsed uint64
		if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil {
			beforeID = uint(parsed)
		}
	}

	iccid := requestedICCID
	if requestedICCID != "" && (strings.TrimSpace(deviceID) != "" || strings.TrimSpace(imsi) != "") {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "iccid 不能与 device_id 或 imsi 同时使用"})
		return
	}
	if requestedICCID == "" && (strings.TrimSpace(deviceID) != "" || strings.TrimSpace(imsi) != "") {
		resolved, status, msg := s.resolveSMSICCID(deviceID, imsi)
		if status != 0 {
			c.JSON(status, gin.H{"status": "error", "message": msg})
			return
		}
		iccid = resolved
	}

	list, err := db.GetSMSByICCIDAndPeer(iccid, peer, limit, beforeTs, beforeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "查询数据库失败: " + err.Error()})
		return
	}

	devName := s.smsDeviceInfoByICCID()[iccid].name

	enriched := make([]SMSWithDevice, 0, len(list))
	for _, sms := range list {
		enriched = append(enriched, SMSWithDevice{
			SMS:        sms,
			DeviceName: devName,
		})
	}

	c.JSON(http.StatusOK, enriched)
}

func (s *Server) handleDeleteSMSMessage(c *gin.Context) {
	id64, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "无效的短信 id"})
		return
	}

	threadEmpty, imsi, peer, err := db.DeleteSMSByID(uint(id64))
	if err != nil {
		if errors.Is(err, db.ErrSMSNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "短信不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "删除短信失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"thread_empty": threadEmpty,
		"imsi":         imsi,
		"peer":         peer,
	})
}

func (s *Server) handleDeleteSMSThread(c *gin.Context) {
	deviceID := c.Query("device_id")
	imsi := c.Query("imsi")
	iccid := db.CanonicalICCID(c.Query("iccid"))
	peer := strings.TrimSpace(c.Query("peer"))
	if peer == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "缺少 peer 参数"})
		return
	}

	resolved := iccid
	if iccid != "" && (strings.TrimSpace(deviceID) != "" || strings.TrimSpace(imsi) != "") {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "iccid 不能与 device_id 或 imsi 同时使用"})
		return
	}
	if iccid == "" {
		var status int
		var msg string
		resolved, status, msg = s.resolveSMSICCID(deviceID, imsi)
		if status != 0 {
			c.JSON(status, gin.H{"status": "error", "message": msg})
			return
		}
	}

	deleted, err := db.DeleteSMSByICCIDAndPeer(resolved, peer)
	if err != nil {
		if errors.Is(err, db.ErrSMSNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "短信会话不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "删除短信会话失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"deleted": deleted,
		"iccid":   resolved,
		"peer":    peer,
	})
}

// ---------------- 鉴权与静态服务 ----------------

func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}

	clientIP := c.ClientIP()
	if !s.allowLoginAttempt(clientIP, time.Now()) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"status":     "error",
			"code":       "rate_limited",
			"message":    "登录尝试过于频繁，请稍后再试",
			"request_id": requestID(c),
		})
		return
	}

	session, authenticated, err := s.createLoginSession(req.Username, req.Password)
	if authenticated {
		if err != nil {
			logger.Error("生成登录 token 失败", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":     "error",
				"code":       "internal_error",
				"message":    "登录失败",
				"request_id": requestID(c),
			})
			return
		}
		logger.Info("登录成功", "ip", clientIP, "username", req.Username)

		c.JSON(http.StatusOK, gin.H{
			"status":     "ok",
			"token":      session.Token,
			"expires_at": session.ExpiresAt.Format(time.RFC3339),
			"credential": session.Credential,
		})
	} else {
		logger.Warn("登录失败", "ip", clientIP, "username", req.Username)
		c.JSON(http.StatusUnauthorized, gin.H{
			"status":     "error",
			"code":       "invalid_credentials",
			"message":    "用户名或密码错误",
			"request_id": requestID(c),
		})
	}
}

// handleChangePassword 处理修改密码请求
func (s *Server) handleChangePassword(c *gin.Context) {
	var req struct {
		OldPassword     string `json:"old_password"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}

	s.authChangeMu.Lock()
	defer s.authChangeMu.Unlock()
	credentials := s.authSnapshot()
	if credentials.PasswordSource == config.WebPasswordSourceEnvironment {
		c.JSON(http.StatusConflict, gin.H{
			"status":  "error",
			"code":    "password_managed_by_environment",
			"message": "当前密码由环境变量 " + config.WebPasswordEnvironmentVariable + " 管理，请修改部署环境并重启服务",
		})
		return
	}

	// 校验当前密码
	if !checkPassword(credentials.Password, req.OldPassword) {
		c.JSON(http.StatusUnauthorized, gin.H{
			"status":  "error",
			"code":    "invalid_password",
			"message": "当前密码错误",
		})
		return
	}

	// 校验新密码与确认密码一致
	if req.NewPassword != req.ConfirmPassword {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"code":    "password_mismatch",
			"message": "两次输入的新密码不一致",
		})
		return
	}

	// 校验新密码不能为空
	if strings.TrimSpace(req.NewPassword) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"code":    "empty_password",
			"message": "新密码不能为空",
		})
		return
	}
	if isWeakPassword(req.NewPassword, credentials.Username) {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"code":    "weak_password",
			"message": "新密码强度不足：至少 8 位；少于 12 位时需包含至少两类字符，且不能使用常见密码或与用户名相同",
		})
		return
	}

	// 生成 bcrypt 哈希
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		logger.Error("生成密码哈希失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "密码处理失败",
		})
		return
	}
	hashedPassword := string(hashed)

	// 持久化到配置文件
	if err := config.UpdateWebCredentialsInFile(s.configPath, credentials.Username, hashedPassword); err != nil {
		logger.Error("更新密码配置失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"message": "保存配置失败: " + err.Error(),
		})
		return
	}

	// 更新内存中的密码（已哈希）
	s.setAuthPassword(hashedPassword)
	token, exp, err := s.issueSessionToken()
	if err != nil {
		logger.Error("签发新登录 token 失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "密码已保存，但新会话签发失败，请重新登录"})
		return
	}

	logger.Info("密码已更新", "username", credentials.Username, "ip", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"message":    "密码已更新",
		"token":      token,
		"expires_at": exp.Format(time.RFC3339),
		"credential": s.passwordStatus(req.NewPassword),
	})
}

func (s *Server) authorizeRotate(c *gin.Context, username string, password string, now time.Time) bool {
	token := strings.TrimSpace(c.GetHeader("Authorization"))
	if token != "" {
		token = strings.TrimPrefix(token, "Bearer ")
		token = strings.TrimSpace(token)
		if token != "" && s.isSessionTokenValid(token, now) {
			return true
		}
	}

	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" && password == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"status":     "error",
			"code":       "unauthorized",
			"message":    "未授权",
			"request_id": requestID(c),
		})
		return false
	}
	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{
			"status":     "error",
			"code":       "method_not_allowed",
			"message":    "仅支持 POST 表单/JSON 认证",
			"request_id": requestID(c),
		})
		return false
	}

	clientIP := c.ClientIP()
	if !s.allowLoginAttempt(clientIP, now) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"status":     "error",
			"code":       "rate_limited",
			"message":    "请求过于频繁，请稍后再试",
			"request_id": requestID(c),
		})
		return false
	}

	credentials := s.authSnapshot()
	if username == credentials.Username && checkPassword(credentials.Password, password) {
		return true
	}

	c.JSON(http.StatusUnauthorized, gin.H{
		"status":     "error",
		"code":       "invalid_credentials",
		"message":    "用户名或密码错误",
		"request_id": requestID(c),
	})
	return false
}

func (s *Server) isSessionTokenValid(token string, now time.Time) bool {
	decodedBytes, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decodedBytes), ".", 2)
	if len(parts) != 2 {
		return false
	}
	expStr, sig := parts[0], parts[1]

	expInt, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if now.After(time.Unix(expInt, 0)) {
		return false
	}

	h := hmac.New(sha256.New, []byte(s.authSnapshot().Password))
	h.Write([]byte(expStr))
	expectedSig := hex.EncodeToString(h.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expectedSig))
}

func (s *Server) requestSessionToken(c *gin.Context) string {
	token := strings.TrimSpace(c.GetHeader("Authorization"))
	if token == "" {
		return ""
	}
	token = strings.TrimPrefix(token, "Bearer ")
	return strings.TrimSpace(token)
}

func (s *Server) isAuthenticatedRequest(c *gin.Context, now time.Time) bool {
	token := s.requestSessionToken(c)
	return token != "" && s.isSessionTokenValid(token, now)
}

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.requestSessionToken(c) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"status":     "error",
				"code":       "unauthorized",
				"message":    "未授权",
				"request_id": requestID(c),
			})
			c.Abort()
			return
		}

		if s.isAuthenticatedRequest(c, time.Now()) {
			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{
			"status":     "error",
			"code":       "unauthorized",
			"message":    "未授权",
			"request_id": requestID(c),
		})
		c.Abort()
	}
}

func (s *Server) handleStatic(c *gin.Context) {
	requestPath := c.Request.URL.Path

	// 如果是 API 请求但未匹配到路由，返回 404
	if strings.HasPrefix(requestPath, "/api") {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "API 不存在"})
		return
	}

	// 纯后端模式：未启用静态文件系统时，非 API 路径统一返回 404。
	if s.fs == nil {
		c.String(http.StatusNotFound, "Not Found")
		return
	}

	filePath := strings.TrimPrefix(requestPath, "/")
	if filePath == "" {
		filePath = "index.html"
	}

	// 尝试打开文件
	f, err := s.fs.Open(filePath)
	if err != nil {
		// 文件不存在，回退到 index.html (SPA 模式)
		filePath = "index.html"
		f, err = s.fs.Open(filePath)
		if err != nil {
			c.String(http.StatusNotFound, "Not Found")
			return
		}
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		c.String(http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 如果是目录，回退到 index.html
	if stat.IsDir() {
		f.Close()
		filePath = "index.html"
		f, err = s.fs.Open(filePath)
		if err != nil {
			c.String(http.StatusNotFound, "Not Found")
			return
		}
		stat, _ = f.Stat()
	}

	// 设置缓存头
	if filePath == "index.html" {
		c.Header("Cache-Control", "no-cache")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
	} else if strings.HasPrefix(filePath, "assets/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "public, max-age=3600")
	}

	// 设置 Content-Type
	contentType := "application/octet-stream"
	if strings.HasSuffix(filePath, ".html") {
		contentType = "text/html; charset=utf-8"
	} else if strings.HasSuffix(filePath, ".css") {
		contentType = "text/css; charset=utf-8"
	} else if strings.HasSuffix(filePath, ".js") {
		contentType = "application/javascript; charset=utf-8"
	} else if strings.HasSuffix(filePath, ".json") {
		contentType = "application/json; charset=utf-8"
	} else if strings.HasSuffix(filePath, ".svg") {
		contentType = "image/svg+xml"
	} else if strings.HasSuffix(filePath, ".png") {
		contentType = "image/png"
	} else if strings.HasSuffix(filePath, ".ico") {
		contentType = "image/x-icon"
	} else if strings.HasSuffix(filePath, ".woff") {
		contentType = "font/woff"
	} else if strings.HasSuffix(filePath, ".woff2") {
		contentType = "font/woff2"
	}
	c.Header("Content-Type", contentType)

	// 使用 http.ServeContent 直接响应文件内容
	http.ServeContent(c.Writer, c.Request, filePath, stat.ModTime(), f.(io.ReadSeeker))
}

func (s *Server) handleSystemInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":    global.Version,
		"build_time": global.BuildTime,
		"config":     viper.ConfigFileUsed(),
		"docs":       currentAPIDocsLinks(),
	})
}
