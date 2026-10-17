package device

import "testing"

func TestModemVoiceAdapter(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		want     modemVoiceAdapterKind
	}{
		{
			name:     "QDC507 型号走 QDC 流程",
			identity: "QDC507\r\nRevision: QDC507GLEFM21\r\nOK\r\n",
			want:     modemVoiceAdapterQDC507,
		},
		{
			name:     "真机 EC20F 走原生声卡",
			identity: "Quectel\r\nEC20F\r\nRevision: EC20CEFAGR08A03M4G\r\nOK\r\n",
			want:     modemVoiceAdapterNative,
		},
		{
			name:     "EC25 也尝试原生声卡",
			identity: "Quectel\r\nEC25\r\nRevision: EC25EFAR06A03M4G\r\nOK\r\n",
			want:     modemVoiceAdapterNative,
		},
		{
			name:     "空响应当原生处理，由声卡存在性兜底",
			identity: "",
			want:     modemVoiceAdapterNative,
		},
		{
			name:     "QDC507字样出现在非首行也认型号",
			identity: "Manufacturer: QDC507\r\nOK\r\n",
			want:     modemVoiceAdapterQDC507,
		},
		{
			name:     "小写 qdc507 同样认型号",
			identity: "qdc507\r\nOK\r\n",
			want:     modemVoiceAdapterQDC507,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modemVoiceAdapter(tc.identity); got != tc.want {
				t.Fatalf("modemVoiceAdapter(%q) = %q, want %q", tc.identity, got, tc.want)
			}
		})
	}
}

func TestParseATI(t *testing.T) {
	cases := []struct {
		name     string
		response string
		model    string
		firmware string
	}{
		{
			name:     "真机 EC20 三行格式",
			response: "Quectel\r\nEC20F\r\nRevision: EC20CEFAGR08A03M4G\r\nOK\r\n",
			model:    "EC20F",
			firmware: "EC20CEFAGR08A03M4G",
		},
		{
			name:     "单行型号格式",
			response: "QDC507\r\nRevision: QDC507GLEFM21\r\nOK\r\n",
			model:    "QDC507",
			firmware: "QDC507GLEFM21",
		},
		{
			name:     "LF 行尾与多余空行",
			response: "\nQuectel\n\nEC20F\nRevision: EC20CEFAGR08A03M4G\nOK\n",
			model:    "EC20F",
			firmware: "EC20CEFAGR08A03M4G",
		},
		{
			name:     "revision 小写也认",
			response: "Quectel\nEC20F\nrevision: EC20CEFAGR08A03M4G\nOK",
			model:    "EC20F",
			firmware: "EC20CEFAGR08A03M4G",
		},
		{
			name:     "没有 Revision 行时拼接非空行",
			response: "Quectil EC25\r\nOK\r\n",
			model:    "Quectil EC25",
			firmware: "",
		},
		{
			name:     "空响应返回空",
			response: "",
			model:    "",
			firmware: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, firmware := parseATI(tc.response)
			if model != tc.model || firmware != tc.firmware {
				t.Fatalf("parseATI(%q) = (%q, %q), want (%q, %q)", tc.response, model, firmware, tc.model, tc.firmware)
			}
		})
	}
}
