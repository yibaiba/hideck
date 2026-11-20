package smscodec

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	smspdu "github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/tpdu"
	"github.com/warthog618/sms/encoding/ucs2"
)

type RPDUKind string

const (
	RPDUKindUnknown RPDUKind = "UNKNOWN"
	RPDUKindData    RPDUKind = "RP-DATA"
	RPDUKindAck     RPDUKind = "RP-ACK"
	RPDUKindError   RPDUKind = "RP-ERROR"
	RPDUKindSMMA    RPDUKind = "RP-SMMA"
)

type RPDUInfo struct {
	Kind    RPDUKind
	RawType byte
	MR      byte
	Cause   int
}

type SMSEncoding string

const (
	SMSEncodingAuto SMSEncoding = "auto"
	SMSEncodingUCS2 SMSEncoding = "ucs2"
)

type SubmitOptions struct {
	Encoding SMSEncoding
}

func NormalizeSMSEncoding(raw string) (SMSEncoding, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(SMSEncodingAuto):
		return SMSEncodingAuto, nil
	case string(SMSEncodingUCS2):
		return SMSEncodingUCS2, nil
	default:
		return "", fmt.Errorf("unsupported SMS encoding: %s", raw)
	}
}

// DecodeBodyMaybeHex 尝试把 HTTP/SIP 载荷按十六进制字符串解码，否则原样返回。
func DecodeBodyMaybeHex(body []byte) ([]byte, error) {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return nil, errors.New("body 为空")
	}
	if IsHexString(s) {
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return body, nil
}

// IsHexString 判断字符串是否为偶数长度的十六进制编码。
func IsHexString(s string) bool {
	if len(s) < 2 || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// ParseRPData 解析 RP-DATA（RPDU）并提取 RP-MR 与 TPDU。
func ParseRPData(body []byte) (byte, []byte, error) {
	if len(body) < 3 {
		return 0, nil, fmt.Errorf("RPDU 过短")
	}
	i := 0
	i++
	rpMr := body[i]
	i++
	if i >= len(body) {
		return 0, nil, fmt.Errorf("RP-DA 缺失")
	}
	daLen := int(body[i])
	i++
	if i+daLen > len(body) {
		return 0, nil, fmt.Errorf("RP-DA 超界")
	}
	i += daLen

	if i >= len(body) {
		return 0, nil, fmt.Errorf("RP-OA 缺失")
	}
	oaLen := int(body[i])
	i++
	if i+oaLen > len(body) {
		return 0, nil, fmt.Errorf("RP-OA 超界")
	}
	i += oaLen

	if i >= len(body) {
		return 0, nil, fmt.Errorf("RP-UD 缺失")
	}
	udLen := int(body[i])
	i++
	if i+udLen > len(body) {
		return 0, nil, fmt.Errorf("RP-UD 超界")
	}
	tpduBytes := body[i : i+udLen]
	return rpMr, tpduBytes, nil
}

func ClassifyRPDU(body []byte) RPDUInfo {
	if len(body) == 0 {
		return RPDUInfo{Kind: RPDUKindUnknown}
	}
	info := RPDUInfo{
		RawType: body[0],
		Kind:    RPDUKindUnknown,
	}
	if len(body) > 1 {
		info.MR = body[1]
	}
	switch body[0] {
	case 0x00, 0x01:
		info.Kind = RPDUKindData
	case 0x02, 0x03:
		info.Kind = RPDUKindAck
	case 0x04, 0x05:
		info.Kind = RPDUKindError
		if cause, err := ParseRPErrorCause(body); err == nil {
			info.Cause = int(cause)
		}
	case 0x06:
		info.Kind = RPDUKindSMMA
	}
	return info
}

// ParseRPErrorCause 解析 RP-ERROR cause（支持可变长度 Cause IE）。
func ParseRPErrorCause(body []byte) (byte, error) {
	if len(body) < 4 {
		return 0, fmt.Errorf("RP-ERROR 长度不足")
	}
	if body[0] != 0x04 && body[0] != 0x05 {
		return 0, fmt.Errorf("非 RP-ERROR: mti=0x%02x", body[0])
	}
	causeIELen := int(body[2])
	if causeIELen <= 0 {
		return 0, fmt.Errorf("RP-ERROR cause IE 为空")
	}
	if 3+causeIELen > len(body) {
		return 0, fmt.Errorf("RP-ERROR cause IE 越界")
	}
	// 3GPP TS 24.011 cause 为首字节低 7 位，后续诊断字节按需忽略。
	return body[3] & 0x7F, nil
}

func ParseRPDataWithAddresses(body []byte) (byte, string, string, []byte, error) {
	if len(body) < 5 {
		return 0, "", "", nil, fmt.Errorf("RPDU 过短")
	}
	i := 0
	i++
	rpMr := body[i]
	i++

	var oa, da string
	if i >= len(body) {
		return 0, "", "", nil, fmt.Errorf("RP-OA 缺失")
	}
	oaLen := int(body[i])
	i++
	if i+oaLen > len(body) {
		return 0, "", "", nil, fmt.Errorf("RP-OA 超界")
	}
	if oaLen > 0 {
		oa, _ = DecodeAddressValue(body[i : i+oaLen])
	}
	i += oaLen

	if i >= len(body) {
		return 0, oa, "", nil, fmt.Errorf("RP-DA 缺失")
	}
	daLen := int(body[i])
	i++
	if i+daLen > len(body) {
		return 0, oa, "", nil, fmt.Errorf("RP-DA 超界")
	}
	if daLen > 0 {
		da, _ = DecodeAddressValue(body[i : i+daLen])
	}
	i += daLen

	if i >= len(body) {
		return 0, oa, da, nil, fmt.Errorf("RP-UD 缺失")
	}
	udLen := int(body[i])
	i++
	if i+udLen > len(body) {
		return 0, oa, da, nil, fmt.Errorf("RP-UD 超界")
	}
	tpduBytes := body[i : i+udLen]
	return rpMr, oa, da, tpduBytes, nil
}

func DecodeAddressValue(v []byte) (string, error) {
	if len(v) < 1 {
		return "", errors.New("address value 为空")
	}
	ton := v[0]
	bcd := v[1:]

	prefix := ""
	if ton&0x70 == 0x10 {
		prefix = "+" // International
	}

	var sb strings.Builder
	sb.WriteString(prefix)
	for _, b := range bcd {
		lo := b & 0x0F
		hi := (b >> 4) & 0x0F
		if lo <= 9 {
			sb.WriteByte('0' + lo)
		} else if lo == 0x0F {
		} else {
			return "", fmt.Errorf("非法 BCD digit: %x", lo)
		}
		if hi <= 9 {
			sb.WriteByte('0' + hi)
		} else if hi == 0x0F {
		} else {
			return "", fmt.Errorf("非法 BCD digit: %x", hi)
		}
	}
	return sb.String(), nil
}

// EncodeAddress 将号码编码为 LV 格式的 RP-Address（Length + Type + BCD）。
// Length 是 Value 部分（Type + BCD）的字节数。
func EncodeAddress(number string) []byte {
	number = strings.TrimSpace(number)
	if number == "" {
		return []byte{0x00} // Length = 0
	}

	ton := byte(0x81) // Unknown, ISDN/telephone numbering plan
	if strings.HasPrefix(number, "+") {
		ton = 0x91 // International, ISDN/telephone numbering plan
		number = number[1:]
	}

	// BCD 编码：每两个数字一组，低位在前，高位在后
	// 如果是奇数个数字，最后补 F
	length := len(number)
	bcdLen := (length + 1) / 2
	bcd := make([]byte, bcdLen)
	for i := 0; i < length; i++ {
		digit := byte(number[i] - '0')
		if i%2 == 0 {
			bcd[i/2] |= digit
		} else {
			bcd[i/2] |= digit << 4
		}
	}
	if length%2 != 0 {
		bcd[length/2] |= 0xF0
	}

	// RP-Address Value 部分 = Type (1 byte) + BCD
	// RP-Address IE = Length (1 byte) + Value
	totalLen := 1 + len(bcd)
	out := make([]byte, 1+totalLen)
	out[0] = byte(totalLen)
	out[1] = ton
	copy(out[2:], bcd)
	return out
}

// BuildRPData 构造 RP-DATA（RPDU），携带指定 RP-MR 与 TPDU。
// 根据 3GPP TS 24.011 Section 7.3.1：
//   - RP-DATA (MS -> Network): MTI = 000 (0x00)
//   - RP-DATA (Network -> MS): MTI = 001 (0x01)
func BuildRPData(rpMr byte, tpduBytes []byte, smsc string) []byte {
	smscAddr := EncodeAddress(smsc)

	out := make([]byte, 0, 2+1+len(smscAddr)+1+len(tpduBytes))
	out = append(out, 0x00) // RP-Message Type: RP-DATA (MS -> Network)
	out = append(out, rpMr) // RP-Message Reference
	out = append(out, 0x00) // RP-Originator Address Length = 0

	// RP-Destination Address (SMSC)
	out = append(out, smscAddr...) // 包含 Length prefix

	out = append(out, byte(len(tpduBytes))) // RP-User Data Length
	out = append(out, tpduBytes...)         // RP-User Data (TPDU)
	return out
}

// BuildRPAck 构造 RP-ACK（确认收到 RP-DATA）。
// 使用最简格式（仅 MTI 与 MR），避免因附带不兼容的 TPDU 导致部分网关（如 O2 UK）断开下发会话。
func BuildRPAck(rpMr byte) []byte {
	return []byte{0x02, rpMr}
}

// BuildRPSMMA 构造 RP-SMMA（TS 24.011：MS→网络 memory available，MTI 110）。
func BuildRPSMMA(rpMr byte) []byte {
	return []byte{0x06, rpMr}
}

// DummyMSISDN is the 15-zero international MSISDN from 3GPP TS 23.003.
const DummyMSISDN = "+000000000000000"

// RPCauseTemporaryFailure 临时故障（3GPP TS 24.011 §8.2.5.4），SMSC 应稍后重试
const RPCauseTemporaryFailure byte = 41

// RPCauseMemoryCapacityExceeded 内存满（3GPP TS 24.011 §8.2.5.4 cause 22）
const RPCauseMemoryCapacityExceeded byte = 22

// IsDummyMSISDN reports whether value is the TS 23.003 dummy MSISDN.
func IsDummyMSISDN(value string) bool {
	digits := strings.TrimPrefix(strings.TrimSpace(value), "+")
	if len(digits) < 7 || len(digits) > 15 {
		return false
	}
	for _, character := range digits {
		if character != '0' {
			return false
		}
	}
	return true
}

// BuildRPError 构造 RP-ERROR（拒收 RP-DATA，通知 SMSC 投递失败）。
// 根据 3GPP TS 24.011 §7.3.4，RP-ERROR 格式：
//   - MTI (1 byte): 0x04 = RP-ERROR (MS → Network)
//   - MR  (1 byte): 与收到的 RP-DATA 的 RP-MR 对应
//   - Cause IE: Length (1 byte) + Cause value (1 byte)
//   - RP-User Data Length (1 byte): 0（无 TPDU 载荷）
func BuildRPError(rpMr byte, cause byte) []byte {
	return []byte{
		0x04,  // RP-MTI: RP-ERROR (MS → Network)
		rpMr,  // RP-MR
		0x01,  // RP-Cause IE Length = 1
		cause, // RP-Cause value
		0x00,  // RP-User Data Length = 0
	}
}

// ConcatInfo 长短信分片信息（UDH concatenation header）
type ConcatInfo struct {
	IsConcat bool // 是否为多段短信
	Ref      int  // 引用号（同一条长短信的所有分片共享此值）
	RefBits  int  // 引用号位宽：8 或 16
	Total    int  // 总分片数
	Seq      int  // 当前序号 (1-based)
}

type ATDeliverSMS struct {
	Sender           string
	Content          string
	Timestamp        time.Time
	SCTS             string
	SMSC             string
	PID              byte
	DCS              byte
	ApplicationPorts string
	Concat           ConcatInfo
	Binary           bool
}

// DecodeATDeliverSMS decodes a full PDU from an AT storage slot, preserving
// the original SCTS octets used to separate multipart sessions.
func DecodeATDeliverSMS(fullPDU []byte) (ATDeliverSMS, error) {
	if len(fullPDU) == 0 || 1+int(fullPDU[0]) >= len(fullPDU) {
		return ATDeliverSMS{}, errors.New("invalid SMSC length")
	}
	smscEnd := 1 + int(fullPDU[0])
	tpduBytes := fullPDU[smscEnd:]
	dcs, _, udOffset, _, alphabet, ok := deliverTPDULayout(tpduBytes)
	if !ok {
		return ATDeliverSMS{}, errors.New("invalid SMS-DELIVER TPDU layout")
	}
	part := ATDeliverSMS{
		SCTS:   hex.EncodeToString(tpduBytes[udOffset-8 : udOffset-1]),
		SMSC:   hex.EncodeToString(fullPDU[:smscEnd]),
		PID:    tpduBytes[udOffset-10],
		DCS:    dcs,
		Binary: alphabet == tpdu.Alpha8Bit,
	}
	var err error
	var ports udhPorts
	part.Sender, part.Content, part.Timestamp, part.Concat, err = decodeDeliverTPDU(tpduBytes, &ports)
	if err == nil && (ports.Has8Bit || ports.Has16Bit) {
		part.ApplicationPorts = fmt.Sprintf("%t:%d:%d:%t:%d:%d", ports.Has8Bit, ports.DestPort8, ports.SrcPort8,
			ports.Has16Bit, ports.DestPort16, ports.SrcPort16)
	}
	return part, err
}

// DecodeDeliverTPDU 解码下行短信 TPDU，返回发送方号码、文本内容、发送时间、和 concat 分片信息。
// 如果 TPDU 包含 UDH concatenation header（长短信分片），concat.IsConcat 为 true。
func DecodeDeliverTPDU(tpduBytes []byte) (sender string, text string, ts time.Time, concat ConcatInfo, err error) {
	return decodeDeliverTPDU(tpduBytes, nil)
}

func decodeDeliverTPDU(tpduBytes []byte, ports *udhPorts) (sender string, text string, ts time.Time, concat ConcatInfo, err error) {
	if trimmed, ok := TrimDeliverTPDUToDeclaredLength(tpduBytes); ok {
		tpduBytes = trimmed
	}
	if normalized, ok := normalizeDeliverTPDUGSM7SpareBits(tpduBytes); ok {
		tpduBytes = normalized
	}
	t, err := smspdu.Unmarshal(tpduBytes)
	if err != nil {
		return "", "", time.Time{}, ConcatInfo{}, err
	}
	msg, err := smspdu.Decode([]*tpdu.TPDU{t})
	if err != nil {
		return "", "", time.Time{}, ConcatInfo{}, err
	}
	if ports != nil {
		*ports = parseUDHPorts(t.UDH)
	}
	// 检测 UDH 中的 concatenation 信息（长短信分片标识）
	if t.UDH != nil {
		if segments, seqno, mref, ok := t.UDH.ConcatInfo8(); ok && segments > 1 {
			concat = ConcatInfo{IsConcat: true, Ref: mref, RefBits: 8, Total: segments, Seq: seqno}
		} else if segments, seqno, mref, ok := t.UDH.ConcatInfo16(); ok && segments > 1 {
			concat = ConcatInfo{IsConcat: true, Ref: mref, RefBits: 16, Total: segments, Seq: seqno}
		}
	}

	// 检查是否为二进制数据 (比如针对 SIM 卡的 OTA / Class 2 消息)，直接强转会破坏编码导致 webhook 报错
	textStr := string(msg)
	alpha, aErr := t.DCS.Alphabet()
	if aErr == nil && alpha == tpdu.Alpha8Bit {
		classified := classifyBinarySMS(t, msg)
		textStr = formatBinaryClassification(classified)
	}

	// 最终安全保障：滤除任何非法的非 UTF-8 截断内容，防止下游 JSON 序列化崩溃
	textStr = strings.ToValidUTF8(textStr, "")

	if t.SmsType() == tpdu.SmsDeliver {
		return t.OA.Number(), textStr, t.SCTS.Time, concat, nil
	}
	return "", textStr, time.Time{}, concat, nil
}

// IsShortCode 判断号码是否为运营商短号码/服务号码（非标准手机号）
// 短号码特征：无 + 前缀、长度 <= 6 位、纯数字
func IsShortCode(phone string) bool {
	if strings.HasPrefix(phone, "+") {
		return false
	}
	digits := strings.TrimLeft(phone, "0123456789")
	return digits == "" && len(phone) <= 6
}

// BuildSubmitTPDUs 编码上行短信为一组 SUBMIT TPDU（支持长短信切片）。
// 返回 TPDU 字节数组列表 和 对应的长度列表（不含 SMSC），以及可能的错误。
func BuildSubmitTPDUs(to, text string) ([][]byte, []int, error) {
	return BuildSubmitTPDUsWithOptions(to, text, SubmitOptions{})
}

// BuildSubmitTPDUsWithOptions 编码上行短信为一组 SUBMIT TPDU，并允许调用方指定文本编码策略。
func BuildSubmitTPDUsWithOptions(to, text string, opts SubmitOptions) ([][]byte, []int, error) {
	normalizedTo := strings.TrimSpace(to)
	encoding, err := NormalizeSMSEncoding(string(opts.Encoding))
	if err != nil {
		return nil, nil, err
	}

	msg := []byte(text)
	encoderOptions := []smspdu.EncoderOption{smspdu.To(normalizedTo)}
	if encoding == SMSEncodingUCS2 {
		msg = ucs2.Encode([]rune(text))
		encoderOptions = append(encoderOptions, smspdu.AsUCS2)
	}

	tpdus, err := smspdu.Encode(msg, encoderOptions...)
	if err != nil {
		return nil, nil, err
	}
	if len(tpdus) == 0 {
		return nil, nil, errors.New("TPDU 编码结果为空")
	}

	var bytesList [][]byte
	var lenList []int

	for _, pdu := range tpdus {
		// 编码库默认使用国际地址类型；只有显式 + 前缀才代表国际格式。
		if !strings.HasPrefix(normalizedTo, "+") {
			da := pdu.DA
			da.SetTypeOfNumber(tpdu.TonUnknown)
			da.SetNumberingPlan(tpdu.NpISDN)
			pdu.DA = da
		}

		b, err := pdu.MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		bytesList = append(bytesList, b)
		lenList = append(lenList, len(b))
	}

	return bytesList, lenList, nil
}
