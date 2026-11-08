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
	"github.com/yibaiba/hideck/internal/mcpserver"
	"github.com/yibaiba/hideck/internal/notify"
	"github.com/yibaiba/hideck/internal/phone"
	"github.com/yibaiba/hideck/internal/proxy/server"
	proxytraffic "github.com/yibaiba/hideck/internal/proxy/traffic"
	"github.com/yibaiba/hideck/internal/updater"
	"github.com/yibaiba/hideck/internal/webdavarchive"
	vwebsheet "github.com/yibaiba/hideck/internal/websheet"

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

const (
	loginAttemptWindow = 2 * time.Minute
	loginAttemptLimit  = 10
	loginLockout       = 10 * time.Minute
)

type loginAttempt struct {
	Gen         uint64 // bumped per counting window; a late release for an older window is ignored
	Count       int
	WindowEnd   time.Time
	LockedUntil time.Time
	Warned      bool
}

func (a loginAttempt) expiresAt() time.Time {
	if a.LockedUntil.After(a.WindowEnd) {
		return a.LockedUntil
	}
	return a.WindowEnd
}

const updateCheckTimeout = 10 * time.Second

// Server 是 API 服务器的核心结构
type Server struct {
	mcpMu                   sync.RWMutex
	mcpOnce                 sync.Once
	mcpHandler              *mcpserver.Handler
	mcpError                error
	mcpTokens               *mcpserver.Tokens
	mcpConnections          *mcpserver.Connections
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
	phoneSettingsMu         sync.Mutex
	systemConfigMu          sync.RWMutex
	archiveConfigMu         sync.Mutex
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
	cardPolicyMWIRedetector cardPolicyMWIRedetector
	disclaimerAcceptances   disclaimerAcceptanceStore
	systemTime              systemTimeProvider
	backendSwitch           deviceBackendSwitcher
	updateChecker           systemUpdateChecker
	archive                 archiveManager

	httpSrvMu sync.Mutex
	httpSrv   *http.Server
	httpsSrv  *http.Server

	loginMu                sync.Mutex
	loginAttempts          map[string]loginAttempt
	loginPrunedAt          time.Time
	loginGen               uint64
	authMu                 sync.RWMutex
	authChangeMu           sync.Mutex
	passwordChangeRequired bool

	shutdownCh       chan struct{}
	backgroundCancel context.CancelFunc
}

type realtimeTrafficSubscriber interface {
	Subscribe(ctx context.Context, deviceID string) (<-chan proxytraffic.RealtimeSnapshot, func())
}

type archiveManager interface {
	Config() webdavarchive.RuntimeConfig
	Status() webdavarchive.Status
	Configure(webdavarchive.RuntimeConfig) error
	Probe(context.Context, webdavarchive.RuntimeConfig) error
	RunNow(context.Context) (webdavarchive.RunSummary, error)
	PreviewCleanup(context.Context) (webdavarchive.CleanupPreview, error)
	Cleanup(context.Context, string) (webdavarchive.CleanupPreview, error)
	Stop(context.Context) error
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
		shutdownCh:    make(chan struct{}),
	}
	s.initializeMCP()
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

func (s *Server) SetArchiveManager(manager archiveManager) {
	s.archive = manager
}

func (s *Server) SetPhoneService(service *phone.Service) {
	s.phone = service
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

// allowLoginAttempt reserves an attempt before the password is checked, so
// concurrent requests cannot exceed the limit. Call the returned release once
// the password is correct; only failures accumulate. A success does not reset
// the count: concurrent wrong attempts from the same IP already hold their
// reservations, and a reset would hand them another round.
// The attempt that reaches loginAttemptLimit is still allowed and starts a
// loginLockout. When rejected, release is nil and wait is the remaining lockout.
func (s *Server) allowLoginAttempt(ip string, now time.Time) (release func(), wait time.Duration) {
	if ip == "" {
		ip = "unknown"
	}

	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.pruneLoginAttemptsLocked(now)

	cur, ok := s.loginAttempts[ip]
	if !ok || !now.Before(cur.expiresAt()) {
		s.loginGen++
		cur = loginAttempt{Gen: s.loginGen, WindowEnd: now.Add(loginAttemptWindow)}
	}
	if now.Before(cur.LockedUntil) {
		if !cur.Warned {
			cur.Warned = true
			s.loginAttempts[ip] = cur
			logger.Warn("登录尝试过多，已锁定该 IP",
				"ip", ip, "attempts", cur.Count, "locked_until", cur.LockedUntil.Format(time.RFC3339))
		}
		return nil, cur.LockedUntil.Sub(now)
	}
	cur.Count++
	if cur.Count >= loginAttemptLimit {
		cur.LockedUntil = now.Add(loginLockout)
	}
	s.loginAttempts[ip] = cur
	gen := cur.Gen
	return func() { s.releaseLoginAttempt(ip, gen) }, 0
}

// releaseLoginAttempt gives back a reservation from the same counting window
// only, and lifts the lockout that reservation triggered once below the limit.
func (s *Server) releaseLoginAttempt(ip string, gen uint64) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	cur, ok := s.loginAttempts[ip]
	if !ok || cur.Gen != gen || cur.Count == 0 {
		return
	}
	cur.Count--
	if cur.Count < loginAttemptLimit {
		cur.LockedUntil, cur.Warned = time.Time{}, false
	}
	s.loginAttempts[ip] = cur
}

// pruneLoginAttemptsLocked drops expired IPs at most once per counting window
// so the map does not grow without bound.
func (s *Server) pruneLoginAttemptsLocked(now time.Time) {
	if now.Sub(s.loginPrunedAt) < loginAttemptWindow {
		return
	}
	s.loginPrunedAt = now
	for ip, a := range s.loginAttempts {
		if !now.Before(a.expiresAt()) {
			delete(s.loginAttempts, ip)
		}
	}
}

func setLoginRetryAfter(c *gin.Context, wait time.Duration) {
	c.Header("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
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

	r.Any("/mcp", s.serveMCP)

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
	api.Use(s.mbnMutationMiddleware())
	s.registerMCPManagement(api)
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
		s.registerMBNRoutes(api)

		// ===== 短信 =====
		api.POST("/sms/send", s.handleSendSMS)                    // 发送短信（自动选择 AT 或 VoWiFi）
		api.GET("/sms/delivery/:message_id", s.handleSMSDelivery) // 查询发送投递状态
		api.GET("/sms/contacts", s.handleGetSMSContacts)          // 获取短信联系人列表
		api.GET("/sms/notifications", s.handleSMSNotifications)   // 全局未读数量与增量来信提醒
		api.GET("/sms/events", s.handleSMSEvents)                 // 来信 SSE，Last-Event-ID / after_id 补发未读
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
		api.GET("/settings/archive", s.handleGetArchiveSettings)
		api.PUT("/settings/archive", s.handleUpdateArchiveSettings)
		api.POST("/settings/archive/test", s.handleTestArchiveSettings)
		api.POST("/archive/actions/run", s.handleRunArchive)
		api.GET("/archive/cleanup/preview", s.handlePreviewArchiveCleanup)
		api.POST("/archive/cleanup", s.handleArchiveCleanup)
		api.GET("/settings/phone", s.handleGetPhoneSettings)
		api.PUT("/settings/phone/auto-answer/devices/:device_id", s.handleUpdateAutoAnswerDevice)
		api.POST("/settings/phone/auto-answer/audio", s.handleUploadAutoAnswerAudio)
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
	return path == "/mcp" || path == "/api/command-center/stream" || path == "/api/commands/events/stream" || path == "/api/phone/events" || path == "/api/sms/events"
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.mcpConnections != nil {
		s.mcpConnections.Clear()
	}
	var archiveErr error
	if s.archive != nil {
		archiveErr = s.archive.Stop(ctx)
	}
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
		return errors.Join(automaticTaskErr, phoneErr, archiveErr)
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
	result := errors.Join(automaticTaskErr, phoneErr, archiveErr)
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

func requestID(c businessContext) string {
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
	s.useListDevices(c)
}

// handleDeviceRescan 手动触发设备重新扫描
func (s *Server) handleDeviceRescan(c *gin.Context) {
	s.useDeviceRescan(c)
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
	s.useLogHistory(c)
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
	releaseMutation := s.pool.AcquireManagedDeviceMutation(deviceID)
	defer releaseMutation()

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
	s.useDeviceMgmtStartNetwork(c)
}

func (s *Server) handleDeviceMgmtStartNetworkLocked(c *gin.Context, deviceID string) {
	s.useDeviceMgmtStartNetworkLocked(c, deviceID)
}

func (s *Server) handleDeviceMgmtStopNetwork(c *gin.Context) {
	s.useDeviceMgmtStopNetwork(c)
}

func (s *Server) handleDeviceMgmtStopNetworkLocked(c *gin.Context, deviceID string) {
	s.useDeviceMgmtStopNetworkLocked(c, deviceID)
}

func (s *Server) handleHealth(c *gin.Context) {
	s.useHealth(c)
}

func (s *Server) handleSendSMS(c *gin.Context) {
	s.useSendSMS(c)
}

type routedSMSRecord struct {
	Identity  device.SMSIdentity
	Sender    string
	Recipient string
	Content   string
	Status    int
}

func saveRoutedSMSRecord(record routedSMSRecord) error {
	if strings.TrimSpace(record.Identity.ICCID) == "" || strings.TrimSpace(record.Identity.IMSI) == "" {
		return db.ErrSMSIdentityUnknown
	}
	return db.SaveSMSForIdentity(db.SMSRecord{
		Identity: db.SMSIdentity{ICCID: record.Identity.ICCID, IMSI: record.Identity.IMSI},
		Sender:   record.Sender, Recipient: record.Recipient, Content: record.Content,
		Type: 2, Status: record.Status, Timestamp: time.Now(),
	})
}

func (s *Server) handleSMSDelivery(c *gin.Context) {
	s.useSMSDelivery(c)
}

// handleVoWiFiEnable 为指定设备启用 VoWiFi
func (s *Server) handleVoWiFiEnableLocked(c *gin.Context, deviceID string) {
	s.useVoWiFiEnableLocked(c, deviceID)
}

// handleVoWiFiDisable 禁用 VoWiFi，保留当前射频/网络状态
func (s *Server) handleVoWiFiDisableLocked(c *gin.Context, deviceID string) {
	s.useVoWiFiDisableLocked(c, deviceID)
}

// handleStatusDetail 返回单个设备的详细状态
func (s *Server) handleStatusDetail(c *gin.Context) {
	s.useStatusDetail(c)
}

type SMSContactWithDevice struct {
	db.SMSContact
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	LocalPhone string `json:"local_phone"` // 当前卡的有效本机号码（手动设置优先）
}

func (s *Server) handleGetSMSContacts(c *gin.Context) {
	s.useGetSMSContacts(c)
}

func smsContactLocalPhone(contact db.SMSContact, index db.SIMPhoneNumberIndex) string {
	if iccid := db.CanonicalICCID(contact.ICCID); iccid != "" {
		return index.ByICCID[iccid]
	}
	return index.ByIMSI[strings.TrimSpace(contact.IMSI)]
}

func (s *Server) handleGetSMSThread(c *gin.Context) {
	s.useGetSMSThread(c)
}

func (s *Server) handleDeleteSMSMessage(c *gin.Context) {
	s.useDeleteSMSMessage(c)
}

func (s *Server) handleDeleteSMSThread(c *gin.Context) {
	s.useDeleteSMSThread(c)
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
	release, wait := s.allowLoginAttempt(clientIP, time.Now())
	if release == nil {
		setLoginRetryAfter(c, wait)
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
		release()
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
	release, wait := s.allowLoginAttempt(clientIP, now)
	if release == nil {
		setLoginRetryAfter(c, wait)
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
		release()
		return true
	}
	logger.Warn("认证失败", "ip", clientIP, "username", username)

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
	s.useSystemInfo(c)
}
