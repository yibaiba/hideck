# VoWiFi 协议对齐与验证范围

本文记录 HiDeck VoWiFi 实现与规范条款、代码入口和自动化测试的对应关系。它用于工程追踪，不替代 GCF、PTCRB 或运营商实验室认证。

## 规范基线

- 本次 SIP/SDP 对齐基线固定为 GSMA IR.51 v10.0 与 GSMA IR.92 v21.0；后续版本升级必须同步更新本表和对应回归测试。
- GSMA IR.51：IMS over untrusted Wi-Fi，VoWiFi IMS 功能继承 IR.92 要求。
- GSMA IR.92：IMS voice and SMS profile，包含 SIP precondition、会话定时器、语音媒体和 SMS over IP 要求。
- 3GPP TS 24.302：EPC via non-3GPP access。
- 3GPP TS 24.229：IMS SIP/SDP Stage 3。
- 3GPP TS 24.341：SMS over IP Stage 3。
- 3GPP TS 33.402、TS 33.203：非 3GPP 接入和 IMS 接入安全。
- IETF RFC 3261、3262、3311、3312、4028：SIP、100rel/PRACK、UPDATE、precondition 和 session timer。

具体部署仍以 SIM 归属运营商的 IMS 配置和启用策略为准；规范允许运营商关闭部分可选能力。

## 追踪矩阵

### ePDG 内部地址分配失败

- `INTERNAL_ADDRESS_FAILURE (36)` 保留为结构化错误，经 ePDG 等待层传给 runtime 重连循环，不再靠错误字符串判断。
- 按 [RFC 7296 §2.21.2](https://www.rfc-editor.org/rfc/rfc7296.html#section-2.21.2) / [§3.15.4](https://www.rfc-editor.org/rfc/rfc7296.html#section-3.15.4)，地址分配失败不一定删除已完成认证的 IKE SA。本实现选择 Delete 后重新建立：仅最终受保护响应、已完成互相 EAP 认证的 36，在关闭本次候选隧道前发送 IKE Delete，并等待匹配的受保护空响应；不删除未建立的 CHILD_SA。严格 AUTH 校验开启时，未通过校验不发送该 Delete；关闭 EAP MAC 校验的诊断模式也不启用它。
- Delete 交换复用现有 IKE 重传机制，另设本地 5 秒清理预算。写出失败、响应无效或超时与原始 36 一并上报；只有验证响应后才记录 `IKE address rejection cleanup acknowledged`。这不证明网侧地址池或所有历史会话已经恢复。
- 仅结构化 36 的 runtime 重试使用独立的 **2～4 分钟随机等待**，且不缩短调用方更长的重试间隔或已有 `RetryAt` 期限；关闭/取消立即停止等待。RFC 建议等待数分钟，2～4 分钟与 5 秒均为本项目实现参数，不是规范固定值。
- SOCKS5 主隧道遇到结构化 36 后，在上述等待结束的下一轮启动中重新解析 ePDG，优先选择本轮尚未返回 36 的 DNS 地址。历史由当前 runtime 持有，按身份、APN、ePDG 端点、DNS 和代理入口隔离，不写入 P-CSCF 降权表；成功建立隧道后清空。单地址或所有当前候选均返回 36 时，下次延迟重试开始新一轮，不永久拉黑、不立即遍历。DNS 顺序变化不会跳过尚未尝试的地址。
- ePDG 候选轮换是本项目恢复策略，不是 RFC 对 Notify 36 的强制动作，也不保证网侧分配恢复。SOCKS5 使用选中的传输 IP，但保留原始 FQDN、APN 和认证参数；直连保留 SocketManager 自带的多地址发送/响应锁定行为，不被固定为单地址。未启用 `swu.Config.EPDGCandidates` 的独立 SWu 调用保持原选择行为。附加 XCAP PDN 不继承主隧道候选历史，重叠旧候选的迟到结果不能改写新尝试。
- 不改变 VOXI RST/488、普通 EOF、2degrees 按需 port-s、SIP Retry-After/RFC 5626 退避。附加 XCAP PDN 或重叠重认证候选失败，不因此拆掉仍健康的主会话；候选 Delete 仅针对本次 IKE SA。
- 回归：`session_auth_failure_test.go`、`epdg_candidates_test.go`、`session_epdg_candidate_test.go`、`session_epdg_socks_test.go`、`manager_test.go`、`ike_address_retry_test.go`、runtime 状态传递及 XCAP 隔离测试。本轮为本地协议/状态测试，尚未部署真卡验证，不能据此认定运营商侧 36 的根因已消除。

### 能力矩阵

| 能力 | 规范依据 | 实现入口 | 自动化证据 | 状态 |
| --- | --- | --- | --- | --- |
| ePDG 发现与非 3GPP 接入 | TS 24.302、IR.51 | `startup.SelectEmergencyEPDG`、preset FQDN、A/AAAA | 对应包测试 | **部分实现**：preset 或 override 的 FQDN 加 A/AAAA，没有 DNS NAPTR 动态发现。`internal/vowifi/dns` 只服务 IMS Registrar。AAAA-only ePDG 加 IPv4-only SOCKS5 代理不可达，属已知约束。 |
| IKEv2、EAP-AKA/AKA'、IPsec SA | TS 33.402、RFC 7296、RFC 4187/5448 | `ikev2`、`ipsec`、`swu` | 对应包测试 | 已实现 |
| IMS AKA 注册与安全协商 | TS 24.229、TS 33.203 | `imscore/register*`、`sec_agree*` | 注册、AKA、UDP/TCP/IPsec 回归测试 | 已实现 |
| 可靠临时响应 | RFC 3262、TS 24.229 | `voice/outbound_provisional.go` | `TestAgentPRACKsReliableProvisionalBeforeFinalInvite` | 已实现 |
| VoWiFi SIP precondition 状态更新 | IR.51 2.4.1、IR.92 2.4.1、RFC 3312 | `voice/outbound_precondition_update.go` | `TestAgentSendsPreconditionUpdateAfterReliableProvisional`、`TestLocalClientOwnsReliableProvisionalPRACK` | 已实现 |
| precondition UPDATE 编码收敛 | IR.92 2.4.1 | `voice/sdp_codec_selection.go` | `TestBuildPreconditionStatusSDPUsesSelectedCodecsAndQoS` | 已实现 |
| SIP OPTIONS 与 Allow 能力声明 | RFC 3261 11、20.5 | `imscore/inbound_options.go`、`transport_runtime.go` | `TestBuildInboundOPTIONSResponseAdvertisesCapabilities` | 已实现 |
| 呼叫、ACK/CANCEL、保持与恢复 | TS 24.229、TS 24.610、RFC 3261 | `voice`、`voice/agent_calls.go` `SwitchCall`、`internal/api/phone.go` | `TestSecondOutboundCallAllowedWhenFirstIsConnected`、hold/CSeq 回归 | **部分实现**：hold/resume 发 TS 24.610 re-INVITE。DEV-27 已落地：最多两路已应答用户通话，`SwitchCall` 切换，第三路 busy；电话 API 有 `/phone/calls/:id/switch`。网关仍可能返回 `ErrHoldNotAligned`，电话页映射为 `ErrHoldUnavailable`。CANCEL/BYE 带 `Reason: RELEASE_CAUSE`。电话页没有 merge/transfer 端点。 |
| SIP session timer | RFC 4028、IR.92 2.2.8 | `voice/session_timer.go` | session timer 与 422/UPDATE/re-INVITE 测试 | 已实现 |
| SMS over IMS | TS 24.341 | `sms`、`imscore/sms*` | RP-DATA、RP-ACK、状态报告和 SMMA 测试 | **部分实现**：MT 在 SMSC 缺失时仍可 Ready；MO 需要 SMSC。TP-SRR 可置位。 |
| IMS 紧急注册、紧急 URI 和 emergency ePDG 选择 | TS 24.229、IR.51 | `imscore/emergency_register.go`、`voice/emergency*`、`startup.SelectEmergencyEPDG` | emergency 单元测试 | 协议构造已实现但默认禁用；普通启动和拨号路径不调用 |
| P-Access-Network-Info `i-wlan-node-id` | TS 24.229 7.2A.4.3 NOTE 3 | `imscore/helpers.go` `GenerateStableWlanNodeID` | `TestGenerateStableWlanNodeID`、`TestGeneratePAccessNetworkInfoPrefersRealBSSID` | **已知偏差**：本形态无 802.11 关联，使用身份派生的本地管理 MAC；若宿主能读到真实 BSSID 则优先使用。不得写成已对齐。该值也是紧急定位标识，合成值不可用于 PSAP 定位。 |
| Cellular-Network-Info | TS 24.229 R.3.1.1A / 7.2.15.1 | `imscore/helpers.go` `GenerateDefaultCellularNetworkInfo` | `TestGenerateDefaultCellularNetworkInfoOmitsSyntheticCell` | VoWiFi 关射频时省略该头；有真实小区时用 `FormatCellularNetworkInfo`。禁止随机 TAC/CellID。 |
| P-CSCF 503 failover | IR.92 2.2.1 / IR.51 4.9 | `imscore/pcscf_recovery.go` `decidePCSCF503Recovery` | `TestDecidePCSCF503RecoveryFollowsTimerB` | 无 Retry-After 时换 P-CSCF 并重新初始注册。Retry-After ≤ Timer B 则等待。Retry-After > Timer B 时仍切换并标记不可用，避免主叫 INVITE 卡死；这是刻意容错，不是规范最小集。 |
| INVITE forking 早期对话 | TS 24.229 5.1.3 / IR.92 2.2 | `imscore/sip_client_legacy.go` `retainClientInviteEarlyDialog` | `TestRetainClientInviteEarlyDialogKeepsForkedTags`、`Test199ClosesOnlyMatchingEarlyDialog` | 不同 To-tag 的 18x 并行保留早期对话，不再 latest-wins。199 只关对应 To-tag。2xx 仍会收拢到确认对话。 |

| 紧急业务 | IR.51 5.3 / TS 24.229 5.1.6 | `emergency_register.go`、`AllowEmergencyRegistration` | emergency 单元测试 | **协议构造就绪，生产禁用**。默认不启紧急 ePDG、不拨 PSAP。PANI 为合成值，无 Geolocation/PIDF-LO，不得声称真实位置上报。 |
| XCAP Ut 补充业务 | IR.92 2.3.2 / TS 24.623 | `xcap`、`runtimehost/ut.go` `UtAccess`、`internal/api/ut.go`、`web/src/views/UtServices.vue` | `TestGetFallsBackToSecondXUIOn404`、`TestUtGetAndPutUseRealXCAPDocument`、`TestUtAccessDoesNotUseIMSWhenXCAPRequired` | XCAP GET/PUT simservs，If-Match，404 换 XUI。独立 `xcap_apn` 时 IMS 起来后启动第二条 SWu，Ut HTTP 经该 PDN 拨号，不回退 IMS。无 XCAP PDN 时页面显示真实错误，不用 USSI。桌面/移动浏览器证据缺失（MCP 不可用），仅有单元测试。 |
| 临时会议 / ECT / 入向 Replaces | IR.92 2.3.3 / 2.3.11、RFC 3891 | `voice/conference.go` `MergeConference`、`voice/ect.go` `TransferConsultative`、`voice/replaces.go` | `TestMergeConferenceInvitesFactoryAndRefersBoth`、`TestTransferConsultativeSendsREFERAndReleases`、`TestInboundReplacesTerminatesMatchedDialog` | **协议已实现**（DEV-27/DEV-26 之后）。会议：向工厂 URI INVITE、REFER 两路、SUBSCRIBE `conference-info`；缺工厂 URI 返回 `ErrConferenceFactoryUnavailable`。咨询式转接：出向 REFER 带 Replaces，等 NOTIFY sipfrag 后 BYE 两路；缺 tag 返回 `ErrECTRequiresReplaces`，不静默盲转。入向 INVITE Replaces 按 call-id/to-tag/from-tag 定位并终止匹配 dialog。Supported 含 `replaces` 与解析路径一致。电话 API 没有 merge/transfer 端点。 |
| 多 PDN / 第二 SWu | IR.51 4.5 / 4.7.4 | `swu.SessionManager.StartSlot`、`runtimecore.StartAdditionalPDNs`、`attachAdditionalPDNs` | `TestSessionManagerOverlappingSlotKeepsDefault`、`TestAdditionalPDNsOnlyWhenXCAPAPNDiffers`、`TestStartAdditionalPDNsWaitsThenStopsFailedSlot` | 默认仍是每设备一条 IMS 会话。配置了不同于 IMS APN 的 `xcap_apn` 时，IMS 起来后在同一 ePDG 上并行第二条 SWu；失败隔离，不拆 IMS。 |
| Annex B 动态下发 | IR.51 Annex B / TS.32 | 静态 preset `annex_b.go` | `TestMergeAnnexBAppliesValidFields` | **不支持 OMA-DM/ANDSF**。四个 Annex B 字段只能静态配置；非法值记入 `AnnexBRejection` 且不改默认。 |
| IKE 重叠重认证 | RFC 7296 2.8.3 | `legacy_lifecycle.go`、`runtime_reauth.go` | `TestOverlappingReauthKeepsOldSessionUntilSuccessorIsUp`、`TestOverlappingReauthOmitsInitialContact` | 主机在旧 SA 仍转发时启动新的 IKE_SA_INIT/IKE_AUTH；新 IKE 与 Child SA 起来后再 Delete 旧 SA；新 AUTH 省略 INITIAL_CONTACT 并携带 FastReauthID。新 runtime 失败则保留旧 SA。 |

## SHOULD / OPTIONAL 已知偏差

- 媒体为隧道内明文 RTP，不是 SRTP（OPTIONAL）。
- SIP 信令无 DSCP 差异化；仅 RTP 标 EF 46（SHOULD）。
- 无 RTCP-XR（OPTIONAL）。
- MO 短信经 `smsSendMu` 串行化。
- 入向 Privacy/TIR 未处理（TS 24.608）。

## IMS 订阅拒绝与注册生命周期

- REGISTER 刷新不重建已开始的 `reg` / `message-summary` 订阅，也不清除拒绝原因。有效订阅沿用对话并按协商有效期刷新；`reg` 初始注册新 Contact 后建立新订阅，见 [TS 24.229 §5.1.1.3](https://www.etsi.org/deliver/etsi_ts/124200_124299/124229/18.10.00_60/ts_124229v181000p.pdf)。短有效期采用半周期刷新，避免提前量大于有效期时立即循环发送。
- MWI 收到 405/489 后不重试，直到公共用户身份注销，见 [TS 24.606 §4.7.2.1](https://www.etsi.org/deliver/etsi_ts/124600_124699/124606/18.00.00_60/ts_124606v180000p.pdf)。记录按设备、IMS 身份隔离，在同一次运行的自动恢复和 IKE 重鉴权间共享；TCP 关闭或 Service 销毁不等于注销。确认注销对应绑定后移除绑定；仍有其他已知有效绑定则保留拒绝记录，最后一个已知绑定注销或到期后才结束该记录。不写入永久运营商黑名单。
- `reg` 初始拒绝不随普通 REGISTER 刷新重试。403、超时或 5xx 不写入 MWI 的“不支持”记录；403 保留原有本地拒绝处理，超时和 5xx 保留原有重试处理。订阅结果带注册上下文校验，旧响应不得覆盖新对话状态。
- 失败原因与跳过原因保留在诊断中；本项不改变 port-s 普通 EOF、2degrees 按需下行、VOXI RST/短信回执 488 或 P-CSCF 退避策略。回归入口：`subscription_lifecycle_test.go`、`subscription_registration_store_test.go`。

### SUBSCRIBE / NOTIFY 状态与计时

- `reg` / `message-summary` 的 NOTIFY 按注册上下文、Call-ID、双方 tag 和 Event usage 匹配；不匹配返回 481，不再修改当前订阅或处理其中的注册/语音信箱状态。其他 Event 的原有分发路径保持不变。待发送事务与实际发送状态分开，允许合法 NOTIFY 先于 SUBSCRIBE 200 到达。首个 NOTIFY 确认订阅对话，后到的其他分支响应不得覆盖它。
- 新的对话内 SUBSCRIBE 在分派前保留递增 CSeq，失败也不复用；同一 SIP 事务重传仍使用原请求。电话 ACK/CANCEL 的序号处理不受影响，见 [RFC 3261 §12.2.1.1](https://www.rfc-editor.org/rfc/rfc3261.html#section-12.2.1.1)。
- `active` / `pending` NOTIFY 的 `expires` 是有效期依据；后到的 200 不覆盖同次尝试已收到的 NOTIFY 有效期，0 不回退为正数。可恢复刷新失败保留原协商到期时间，订阅终止类响应则结束该 usage。
- 初始、刷新和取消订阅在出队发送时启动 `Timer N = 64 × T1`，收到匹配 NOTIFY 后取消。定时器到期只结束订阅并记录错误，不清除 IMS/SMS 就绪状态、不触发 runtime 重建。主动取消后保留最终 NOTIFY 的接收窗口，不自动恢复该订阅。
- `terminated;reason=deactivated/timeout` 可立即以新对话重订阅；`probation/giveup` 和其他原因遵守有效的 `retry-after`；`rejected/noresource/invariant` 不自动重订阅。MWI 405/489 的身份级拒绝记录仍优先，普通 REGISTER 刷新和 EOF 不能绕过。未给出重试时间时按本地失败退避：`reg` 30 秒、1、2、4 分钟，之后每 5 分钟；MWI 5、10、20 分钟，之后每 30 分钟。可恢复的 SUBSCRIBE 失败和 Timer N 到期同样适用。这是本地调度策略，不是 RFC 规定的固定重试时长；网络给出的 `Retry-After` / `retry-after` 是下限，见 TS 24.229 §5.1.2.2。失败次数按身份和事件包记录，跨 Service 重建保留、不持久化，只有匹配的 `active/pending` NOTIFY 清零本地退避，未到期的网络等待保留；失败的尝试还在等重试时，仍有效的旧订阅发来的 NOTIFY 不清零。同一个 SUBSCRIBE 事务超时后又遇到 Timer N 到期，只计一次失败。重新完成初始注册后 `reg` 按 TS 24.229 §5.1.1.3 立即订阅一次，再失败接着退避；MWI 继续等待，其 `rejected/noresource/invariant` 也跨重新注册保留。回归入口：`subscription_retry_backoff_test.go`。
- `reg` / MWI 各自记录订阅代次：终止或到期会使旧代次失效，构建中、排队中的旧请求不得继续发送，晚到响应不得覆盖终止状态、新对话或暂停期限。有效的 481 重订阅显式使用新代次；普通 `active/pending` 更新不会使请求失效，也不丢弃同一 IMS 注册下已接纳的 NOTIFY 正文。回归入口：`subscription_attempt_race_test.go`。
- 订阅本身终止与 reginfo 报告当前 Contact 注销是不同事件；后者在确认属于当前上下文后仍进入原来的 IMS 注册恢复流程。
- 已接纳的 NOTIFY 正文按接收顺序处理，不能因后一条到达而丢弃前一条 reginfo 增量，见 [RFC 3680 §5.2](https://www.rfc-editor.org/rfc/rfc3680.html#section-5.2)。`reg` / MWI 队列独立，旧注册上下文的正文仍被隔离；重复通知和重复回调不重复应用。先尝试回复 SIP，再处理正文；回复写失败仍向调用方返回原错误，但不会丢弃已接纳的状态或阻塞后续通知。回归入口：`subscription_notify_queue_test.go`、`subscription_notify_reply_test.go`。

生命周期依据：[RFC 6665 §4.1](https://www.rfc-editor.org/rfc/rfc6665.html#section-4.1)。回归入口：`subscription_protocol_test.go`、`subscription_timer_protocol_test.go`、`subscription_wire_protocol_test.go`。本次未启用最终 IKE_AUTH 的全局严格认证校验；该兼容选项仍需单独验证，不能据本次订阅修复宣称全部 VoWiFi 协议已对齐。

## Vodafone UK / VOXI 的 P-CSCF 恢复策略

此节是 `vodafone_uk_23415` 预设的经验性兼容策略，不是所有运营商必须采用的协议行为。明确 port-s RST 仍使用 5 秒宽限；RP 报告 488 仍触发换路径。普通 EOF 和其他运营商的恢复分支不因此改变。

- **真实 TCP 超时后的升级恢复**：原节点经过 30 秒观察、REGISTER 成功、30 秒下行验证仍失败时，在现有恢复退避到期后替换 IMS/P-CSCF 路径，不再无限重复原节点刷新。两个 30 秒窗口是实现参数，不是 RFC 固定时限；不适用于本地读取期限到期、普通 EOF 或其他预设。gVisor 在网络适配层将传输超时和本地 deadline 分别暴露为 `ETIMEDOUT` / `os.ErrDeadlineExceeded`。
- **取消与代次隔离**：下行验证或退避期间，新 port-s 或当前受保护连接上的有效下行请求会取消未执行的超时切换；REGISTER 响应和旧连接消息不算下行证明。切换提交前重新检查运营节点和 port-s 代次；停止、替换通道会使旧计划失效。迟到的刷新后重连不记录为按需重连能力。
- **恢复任务交接**：并发的旧节点 488、503 或 RST 恢复任务退出时，会交还仍未完成的超时恢复和替代会话验证任务；交接保留原定期限，不重置退避，也不靠额外轮询重试。已有恢复任务占用执行权时，新的 port-s 关闭仍会记录并调度。
- **完整重建后的验证**：故障恢复中新建的 Vodafone UK/VOXI runtime 在 REGISTER 成功后也会启动独立的 30 秒下行验证定时器，不依赖先发生一次 port-s 关闭。未验证记为 `downlink_unverified`，只降低选择优先级，不累计 REGISTER 失败次数。已尝试节点、轮次及下一轮期限跨 runtime 保存；无未尝试的合格候选时，保留仍可用的注册传输并等待整轮随机退避，不每隔 30 秒拆注册。到期可开启下一轮或重新获取节点；新的明确 RST/现有超时升级和当前路径 488 仍可触发对应恢复。普通冷启动和其他运营商不启用此专用策略。周期 REGISTER 不推迟验证期限；当前连接证明恢复、通道替换或主动停止会取消旧定时器。
- **下行证据归属**：诊断用的全局 SIP 请求计数不再作为恢复依据。请求处理前后都校验来源连接、P-CSCF 和传输代次，旧连接迟到完成仍计入处理统计，但不能验证新路径或清零其失败历史。
- **优先级与重试资格分离**：异常节点保留 30 分钟的 `deprioritizedUntil`，有其他合格节点时优先选择其他节点；该记录本身不禁止重试，也不因候选耗尽被删除。
- **独立的 `retryNotBefore`**：替代路径建立失败使用 RFC 5626 §4.5 随机指数退避，Retry-After 只能延长等待。节点尚未到允许重试时间时，即使没有其他候选也不会提前使用。
- **跨重建保留历史**：新隧道使用新下发的 P-CSCF 列表，同时保留重试时间与连续失败次数。没有可选节点时等待最早的重试时间，再进入新的连接尝试；不通过清空记录连续重建。替代 runtime 的初始 REGISTER 失败也参与该恢复计数。
- **下行验证与成功注册分开**：REGISTER 成功并不直接表示 VOXI 短信下行已恢复；提前建立 port-s 也不能在 REGISTER 成功前结束恢复或清零失败次数。两项条件满足后才结束本轮恢复。REGISTER 失败的退避和 Retry-After 归属实际尝试的节点，不归属失败处理后选出的下一候选。验证超时只保留低优先级并调度独立的下行恢复轮次；观察调度沿用首轮随机窗口，不把观察轮次作为 RFC 5626 注册失败次数，不因被动观察而指数延长。实际失败仍按随机指数退避及 Retry-After 处理。退避期内，同一退役路径的重复报告不会按短信数量累计恢复失败次数。其他节点的降权历史仍保留，不把历史记录当作下一次普通断开的恢复触发条件。
- **诊断日志**：`IMS P-CSCF configuration from new tunnel` 记录下发的 IPv4/IPv6 列表；`IMS P-CSCF candidates resolved` 记录前后候选与来源；`IMS P-CSCF candidate eligibility`、`IMS P-CSCF recovery preference and retry scheduled` 分别记录选择结果、降权原因及允许重试时间。重新获取可能仍返回相同节点，不保证产生新的 P-CSCF。

回归入口：`registrar_selection_test.go`、`registrar_recovery_completion_test.go`、`registrar_downlink_watch_test.go`、`registrar_downlink_round_test.go`、`registrar_downlink_round_state_test.go`、`downlink_evidence_test.go`、`pcscf_recovery_handoff_test.go`、`pcscf_recovery_test.go`、`port_s_session_test.go`、`port_s_timeout_recovery_test.go`、`gvisor_tcp_errors_test.go`、`runtimecore_test.go`。退避参考：[RFC 5626 §4.5](https://www.rfc-editor.org/rfc/rfc5626.html#section-4.5)。30 分钟偏好与 VOXI 下行验证仍属于实现策略，不应写成协议规定的黑名单期限。

## 受保护 UDP 下行与分层恢复

- Vodafone UK/VOXI 的替代注册下行验证超时后，若注册传输仍可用且本轮还有另一合格 P-CSCF，只替换 IMS 连接、监听器及 IMS 安全关联，复用现有 ePDG 隧道与 inner IP。注册传输失效、下一轮仅有原节点需要重新发现，以及替代注册实际失败，仍保留已有 runtime 恢复路径；候选遍历、节点退避与 Retry-After 不重置。此调整不改变普通 EOF、2degrees 按需 port-s、VOXI 明确 RST 的 5 秒宽限及 488 恢复策略。
- 受保护端口同时预留 TCP/UDP，防止初始 REGISTER 的 UDP socket 占用通告的 port-c/port-s。仅 IPsec 协商成功后启动 UDP 接收：从协商的 P-CSCF port-c 接收，在本地 port-c 向 P-CSCF port-s 返回 SIP 响应和独立 RP 报告；RP 报告的 Via/传输声明与实际 UDP 路径一致。沿用 SIP 事务和短信去重。参见 [TS 24.229 §3.1、§5.1.1.2.2](https://www.etsi.org/deliver/etsi_ts/124200_124299/124229/18.10.00_60/ts_124229v181000p.pdf)。
- UDP 端口的明文过滤在绑定前安装，覆盖初始分片，不能让预鉴权排队数据绕过安全校验；未知 SPI、完整性失败、重放和不匹配的 UDP 安全关联选择器仍被拒绝。网络适配器通过 `ListenProtectedUDP` 提供该保障；不能提供保障时不启动受保护 UDP 并显式报错，不降级为明文接收。禁用 IPsec 的原有明文模式不受此扩展影响。
- 有效 UDP 下行请求同样可证明当前代次的下行已可用；仅绑定 UDP 端口或收到 REGISTER 响应不算证明。关闭、替换、超时切换提交与下行证据按代次隔离；退役 socket 的迟到数据或错误不可验证、破坏新路径。
- `ipsec_generation`、`previous_ipsec` 和 `last_unknown_inbound` 记录当前/上一套安全关联的标识及最后一个未知 ESP 包的来源、目标、SPI、序号和时间；跨 runtime 可通过 `IMS security association identifiers` 日志关联。未知 SPI 的头部只是未验证观测，不代表短信到达。诊断不保存密钥或短信正文，安装失败保留原有安全关联，成功安装原子替换。
- 回归入口：`protected_udp_test.go`、`registrar_downlink_tunnel_test.go`、`udp_security_test.go`、`ipsec_lifecycle_test.go`。本次没有真卡部署验收，不能据此认定本次 VOXI 故障一定由 UDP 或旧 SPI 引起。

## 系统 IMS 监听器的 MPTCP / MSS 兼容

Linux 的 `SystemIMSNetwork` 先沿用 Go 的 MPTCP 选择（包括 `GODEBUG=multipathtcp`），再对实际 MPTCP 监听 socket 设置 IMS 所需的 `TCP_MAXSEG=1100`。成功则保留原监听器；只有该操作返回 `EOPNOTSUPP` 时，才关闭尚未用于注册的监听器，以普通 TCP 重新监听，并记录 `IMS MPTCP listener cannot set TCP_MAXSEG; retrying with TCP` 警告。不按内核版本推断支持情况，也不修改系统 MPTCP 开关。

能力检查中的其他错误、关闭失败、普通 TCP 重建失败，以及后续 MSS 设置失败都会返回给调用方。普通 TCP、非 Linux 平台和用户态网络适配器保持各自原有路径。此检查只证明监听器支持 MSS，不代表已验证运营商的 MPTCP 多路径互通。

需要禁用这项自动重试、保留原始错误进行诊断时，创建网络适配器后、使用前设置 `network.DisableMPTCPMSSFallback = true`；这是 `SystemIMSNetwork` 的代码级选项，不是 YAML 配置项。回归入口：`network_tcp_linux_test.go`（兼容/不兼容 MPTCP、非兼容性错误、系统实际 socket 选择、禁用重试）和 `tcp_mss_unix_test.go`。

`network_tcp_connection_linux_test.go` 另外检查 IPv4/IPv6 的实际 MPTCP 协商、接受连接上的 MSS 和双向数据传输。2026-10-28 在 Linux 5.18 的 179 主机上验证了不支持 MSS 时使用 TCP；在官方 Ubuntu Linux 7.0.0-42 内核的隔离 QEMU 虚拟机中验证了保留 MPTCP，双方 `MultipathTCP()` 均为 true。测试使用真实内核 socket，但没有验证多条子流、跨网切换或运营商互通。179 的 IMS 全包 699 项通过；新内核虚拟机的 MPTCP/原六项专项通过，全包仍有短时限测试超时，不能标为全包通过。

## 验证边界

下行资源生命周期回归：`port_s_listener_test.go`、`downlink_transport_failure_test.go`、`registrar_downlink_round_state_test.go`、`ipsec_lifecycle_test.go`。当前监听器异常会显式触发恢复，退役监听器与旧 IPsec 清理回调不得破坏新路径；监听器清理与 REGISTER 并发完成、持有注册锁时关闭接收器均有回归覆盖。取消失效连接的被动观察等待不清除真实节点退避，也不能取消重叠重鉴权中新尝试拥有的等待。握手诊断回归：`transport_diagnostics_test.go`、`diagnostics_test.go`、`network_diagnostics_test.go`，覆盖 IPv4/IPv6、双安全流、解密/重放拒绝、写出失败及适配层透传。诊断不改变 SIP/IPsec 报文，不以生成 SYN-ACK 或 REGISTER 200 代替短信接收验收；这些修复不等于已定位运营商侧没有下行的根因。

自动化测试验证消息构造、事务时序、状态迁移和主要失败路径。真实网络还需要分别验证运营商策略、P-CSCF 行为、NAT、IPv4/IPv6、媒体编码和超时参数。只有完成目标运营商的实验室一致性用例后，才能声明通过该运营商认证。
