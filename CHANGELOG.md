# Changelog

## 2.1.22 - 2026-09-13

### 前置代理

- 同一国家多条节点时，开 VoWiFi 优先选 UDP 正常的，不再一直抽到探测失败的那条。
- 选路探测有 2 秒上限；已经有 UDP 正常节点就不再等慢的那条。
- 蜂窝已经能直连时不探国家池。

### 界面 / 通知

- `/list`、`/status` 和仪表盘在 VoWiFi 下显示卡归属（SPN），不再把飞行模式残留的 46001 画成中国联通。
- 未真正驻网时不再把上次的运营商/LTE 当成当前驻网。

## 2.1.21 - 2026-09-12

### 前置代理

- SOCKS5 探测会真的走 UDP 中继做公共 DNS 往返，不只看 ASSOCIATE。
- UDP 探测失败不再拦住保存或开启 VoWiFi；ASSOCIATE 通了就继续，实际链路交给 ePDG/IKE。

## 2.1.20 - 2026-09-10

### 界面

- 仪表盘原生 VoLTE 走 SIM/LTE/PDN/IMS/Voice，不再画成 VoWiFi。
- 仪表盘 `phone_mode` 取运行时，yaml 空字段不会把联通 VoLTE 盖掉。
- VoWiFi 路径把短信发送和接收拆开显示。

### VoWiFi / IMS

- 健康状态按 IMS 实际就绪判断。
- 短信接收降级会单独露出来。

## 2.1.19 - 2026-09-09

### 设备 / PC/SC

- Linux 读卡器句柄按本地 `LONG` 宽度传。
- 没有 IMEI 也能保存读卡器 SIM 身份。
- PIN 只在文件受保护时才校验，失败次数跟当前卡绑定，不再自动连打。

### VoWiFi / IMS

- SIP 地址头里的 service URN 能解析。
- INVITE 失败后 typed-nil SIP 消息不再空指针。

## 2.1.18 - 2026-09-09

### 短信 / IMS

- UDP 上的短信 RP report 走已注册 flow，不再另开一条。
- OpenAPI 补上 VoWiFi 主叫短信就绪字段。

## 2.1.17 - 2026-09-09

### 设备

- Linux 查找 PC/SC 读卡器按本地 `DWORD` 宽度传参，启动扫设备不再把堆打坏。

### 短信 / IMS

- 没有 port-s 接收端也能发 IMS 短信。
- 注册后会重放短信就绪；界面显示 IMS 短信发送就绪。
- IMS 就绪状态串行更新，避免互相覆盖。

## 2.1.16 - 2026-09-08

### 界面

- 中英文可切换，侧栏用 EN / 中。
- 短信打开就停在最新一条，历史按需加载，轮询不再整段重拉。
- 日志顶栏字色跟深色控制台对齐，浅色主题也能看清。

### VoWiFi / IMS

- 不支持的 IMS 订阅会记住；自路由拒绝不落库。
- RP report 488 后等短信重投。
- Vodafone P-CSCF 轮换、下行验证、重发现轮次有上限。
- 受保护 UDP 下行能恢复；SOCKS5 ePDG 地址被拒会换候选。
- 恢复清理只动本轮尝试；监听失败能找回。

### 文档

- README 默认英文，译文放到 `i18n/`。
- 截图换成浅色海军配色，首页用 VoWiFi 已连接状态。

## 2.1.15 - 2026-09-07

### VoWiFi / IMS

- 卡住的受保护下行能恢复。
- 下行证明到了就原子取消超时 failover。

### 打包

- OpenWrt 交叉工具链下载和 job 有超时，不会再把整次发版挂死。

## 2.1.14 - 2026-09-07

### 打包

- 单独提供 OpenWrt 包：`hideck_*_openwrt_amd64` / `arm64` / `armv7`。musl 静态链接，不压 UPX。Debian / 树莓派 OS 仍用原来的 UPX `linux_*` 包。
- `deploy-binary.sh` 在 OpenWrt 上会拉对应静态包，并用 procd 安装。

## 2.1.13 - 2026-09-07

### 短信

- 全局收件箱通知和未读数。
- 通知短信在收件箱窗口外也能定位；历史未读状态会保住。

### VoWiFi / IMS

- port-s 恢复跟注册收尾串行；按运营商和关闭原因收窄 grace。
- 重鉴权候选隔离，RP report 走原来的路径；重叠重鉴权时保住就绪。
- 恢复要等注册和下行都成功才清状态；IMS 订阅在恢复中保住，NOTIFY 按序处理，终止后清掉过期订阅。
- P-CSCF 偏好和重试截止拆开。

### 打包

- GitHub 打 tag 默认 UPX。

## 2.1.12 - 2026-09-06

### VoWiFi / 前置代理

- 卡策略可以钉死某条国家节点，也可以跟国家规则；一国多节点时随机挑。改完会自动重连 WiFi calling。
- 日志能看出走了哪条前置代理。

### VoWiFi / IMS

- P-CSCF 恢复时保住退避，VoWiFi 重试状态会保留，过期的会清掉。
- 被拒的 MT SMS report 能按原来的代理路径恢复。
- SMS MESSAGE 正文处理对齐。

## 2.1.11 - 2026-09-05

### VoWiFi / IMS

- 流被掐后恢复更稳，P-CSCF 退避跟信令节奏对齐。
- WiFi Calling 事件历史有上限，避免把内存撑大。

### 打包

- Docker 等 GitHub Release 二进制编完再推，不再跟编包同时开跑。

## 2.1.10 - 2026-09-04

### 电话 / 联系人

- 来电和短信能显示归属地、运营商和联系人名字。
- 电话页可手动加联系人；支持 iOS、Google、三星、小米、华为、OPPO、vivo 导出的 vcf / csv 导入导出，一人多号会合成一条。
- 联系人侧栏可搜索、批量删、拖拽导入；名单分页下滑加载。

### 通知

- 短信和来电通知带号码、联系人、归属地。

### VoWiFi / IMS

- port-s 被掐后按当前绑定恢复，不乱切 P-CSCF；Vodafone UK 的惩罚记录会保住。
- VOXI 已经建立的 port-s 被 RST 能恢复。
- 原生 VoLTE 启动会合并重复调度。
- WiFi Calling 健康按短信就绪来算。

## 2.1.9 - 2026-09-04

### QMI / 原生 VoLTE

- 换卡后 QMI CTL 超时不再拆掉 worker；身份改从 AT 口读。CTL 连续超时且 AT 还活着时，只复位这一块模组 USB，不用 CFUN。
- `网络` 关闭时主机不上 LTE 默认上网 PDN。VoLTE 的 IMS PDN 起来后也会把 usbnet 刷掉。

### VoWiFi / IMS

- port-s 恢复按 RFC 5626 退避；短信与 IMS 就绪顺序对齐；REGISTER/短信入向恢复更稳。

## 2.1.8 - 2026-09-03

### 打包

- Docker 镜像改回 UPX。UPX 5 的 stub 在 Alpine/gcompat 上会报 `Not a valid dynamic program`，发版底包改为 Debian bookworm（glibc），压缩包可以直接 exec。

## 2.1.7 - 2026-09-03

### VoWiFi / IMS

- 已注册后再发 REGISTER 被 503（2degrees）时，不再当成换传输、也不再拆掉还活着的 TCP。现有绑定保住，port-s 恢复停掉。

### 打包

- Docker 镜像改放未压缩二进制。2.1.6 把 UPX 包塞进 Alpine，容器会报 `Not a valid dynamic program` 一直重启。

## 2.1.6 - 2026-09-03

### VoWiFi / IMS

- port-s 不再发 CRLF。关掉后等 30s，对端不重连就补一次 REGISTER；这次 REGISTER 被拒（如 2degrees 503）就不再恢复。
- outbound 跟进 REGISTER 如果把信令流拆掉，这次注册算失败，马上重来，不再空等保活超时。
- 进程退出不再发 `Expires=0` / `Contact:*` 注销，避免把 IP-SM-GW 一起拆掉、重启后收不到短信。

## 2.1.5 - 2026-09-03

### VoWiFi / IMS

- P-CSCF 关掉 port-s 不再当成注册失败，也不再立刻重 REGISTER。监听还在，等对端重新连上来（#9）。
- RP-ACK 被拒时发 RP-SMMA 问 SMSC 队列；日志会标出哪条短信堵在队头、堵了多久。

## 2.1.4 - 2026-09-02

### 界面

- 默认主题改为 navy-light / navy-night（奶油画布、海军字、强调蓝）。原来的暗色主题仍可在设置里选「经典」。
- 卡片 24px、输入 16px、侧栏选中 12px、主按钮胶囊形。登录仍是左品牌栏 / 右表单，页面结构没改（#5）。

## 2.1.3 - 2026-09-02

### VoWiFi / IMS

- 入向短信按 RFC 3428 回 200。RP-ACK 按 TS 24.341：Request-URI 用 PAI，必带 In-Reply-To 和 binary CTE；488 不再换 URI 重试。
- port-s 入向 TCP 补 30s 套接字保活和 RFC 5626 双 CRLF；对端 RST 后先等重连，不立刻重注册把 Contact 换掉。
- 双栈只在没有可用 IPv6 P-CSCF、或地址不是单播时才跳过 IPv6 Contact。
- CFG_REPLY 能拆开 16390 里拼在一起的 IPv6 P-CSCF。
- 只在 `Require: outbound`、`Path;ob` 或 Contact 带回 `reg-id` 时才补 outbound REGISTER。2degrees 等只广告 `Supported: outbound` 的不再多发一次被 503 拆掉会话（#6 / #8）。
- REGISTER 超时会重试；IPsec SA 被重发时丢掉这次尝试；200 里还有旧 Contact 时先保住当前绑定。

### 修复

- 不再把 `go.work` / `go.work.sum` 纳入版本库。

## 2.1.2 - 2026-09-01

### 修复

- Docker 运行时镜像带上 `libqmi` / `qmi-proxy`，容器里可以走 QMI Proxy，不必再回退直接打开 `/dev/cdc-wdm*`。

## 2.1.1 - 2026-09-01

### eSIM

- Lebara UK 分享卡连过国内网变成 `204/04` 后，不必再删卡重写。同一 ICCID 做停用/启用，或经停车 Profile 中转，连续读到英国 `23487` 后再开 VoWiFi。射频锁不变，也不会把错误身份送去英国 ePDG。

### 原生 VoLTE

- 支持保持/恢复。
- 大疆/佰旺等 USB 声卡不能开时跳过，避免把 QMI 打挂；信令仍可走 VoLTE。
- 中国卡继续走原生 IMS，不走软件 WiFi calling。
- 启动失败会重试；LTE 信号优先显示 RSRP。

### VoWiFi

- 新增一批旅行/常用卡预设：Hotlink、AIS、Smart、Globe、KPN、MTN 等。
- 对端挂断后停止来电振铃。
- IMS REGISTER、短信 RP-ACK、IKE 重鉴权，以及会议 / 转接 / 呼叫等待等协议对齐。

### 修复

- WebRTC 在 NAT 后发布公网 ICE 候选。
- P-CSCF 运行时路径。
- Path 带 `ob` 时 REGISTER 尽快带 `reg-id`。

## 2.1.0 - 2026-08-28

### 原生 VoLTE

- 新增 `phone_mode=volte`：走模组 IMS / QMI VOICE，不建 ePDG，也不走软件 IMS。
- 国内 PLMN 有唯一 MBN 画像时才选择（移动 / 联通 / 电信 / 广电 460-15）；英国等没有唯一画像时不会乱选。
- 通话音频走模组 UAC / ALSA，网页侧仍是 PCMU；接通后可从悬浮栏打开 DTMF。
- 原生来电会通知渠道；挂断会发出 `call_ended`。不会自动 `AT+CFUN` 重启模组。

### VoWiFi 协议

- SMS over IP 按 TS 24.341：Request-URI 走 SMSC PSI，RP-ACK 带 In-Reply-To，对不上回 488；支持 RP-SMMA 和无号码 SMS（Contact 声明 `+g.3gpp.smsip-msisdn-less`）。
- 保活按 RFC 5626 / RFC 6223：TCP 发 `\r\n\r\n`，UDP 发 STUN Binding；只有 `keep=N` 或 `Require: outbound` 才等 pong。
- 语音 SDP 按 IR.92：AMR-WB bandwidth-efficient 在前，octet-align 另开 PT，带 telephone-event 和 `ptime:20`。
- 紧急呼叫只打包不主叫：REGISTER Contact `;sos`、`INVITE urn:service:sos`、`Priority: emergency`，以及配置里的 `sos.epdg...` FQDN。日常 IKE 仍走普通 ePDG，默认不打 PSAP。
- hideck 停止时会回收自己拉起的 qmi-proxy。

### 电话与界面

- 电话页可在模式选择下直接打开 WiFi calling。
- 仪表盘没有 serving operator 时显示 SIM 归属运营商。
- 设备头上的 IMEI / ICCID 可模糊显示。

### eSIM

- 服务端解析激活二维码图片和 PDF。
- 支持从市场激活码 / 拖入的 QR 安装 profile。

### 修复

- 来电/去电结束通知改为中文，与来电通知一致。
- 忽略过期 SIM 快照和 UIM IMSI 读取错误。
- QMI 握手失败时回退 AT 读 IMEI。
- 若干原生通话状态机、声卡枚举和挂断后幽灵振铃问题。
