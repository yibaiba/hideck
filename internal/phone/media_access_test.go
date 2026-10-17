package phone

import (
	"context"
	"strings"
	"testing"
)

const testAnswerSDP = "v=0\r\n" +
	"m=audio 7580 RTP/AVP 0\r\n" +
	"a=candidate:1 1 udp 2130706431 192.168.3.30 7580 typ host\r\n" +
	"a=candidate:2 1 udp 2130706430 172.23.0.1 7580 typ host\r\n" +
	"a=candidate:3 1 udp 2130706429 240e:399:ea2:a970:566d:26cd:f684:8185 7580 typ host\r\n" +
	"a=end-of-candidates\r\n"

func TestPruneAnswerCandidates(t *testing.T) {
	t.Run("局域网地址只留 IPv4 候选", func(t *testing.T) {
		sdp, kept := pruneAnswerCandidates(testAnswerSDP, []string{"192.168.3.30"})
		if kept != 1 {
			t.Fatalf("kept=%d", kept)
		}
		if !strings.Contains(sdp, "192.168.3.30") || strings.Contains(sdp, "172.23.0.1") || strings.Contains(sdp, "240e:399") {
			t.Fatalf("prune wrong: %s", sdp)
		}
		if !strings.Contains(sdp, "a=end-of-candidates") {
			t.Fatalf("sdp structure lost: %s", sdp)
		}
	})
	t.Run("公网 IPv6 访问只留 IPv6 候选", func(t *testing.T) {
		sdp, kept := pruneAnswerCandidates(testAnswerSDP, []string{"240e:399:ea2:a970:566d:26cd:f684:8185"})
		if kept != 1 || !strings.Contains(sdp, "240e:399:ea2:a970:566d:26cd:f684:8185") || strings.Contains(sdp, "192.168.3.30") {
			t.Fatalf("kept=%d sdp=%s", kept, sdp)
		}
	})
	t.Run("域名解析出多个地址时全部保留", func(t *testing.T) {
		_, kept := pruneAnswerCandidates(testAnswerSDP, []string{"192.168.3.30", "240e:399:ea2:a970:566d:26cd:f684:8185"})
		if kept != 2 {
			t.Fatalf("kept=%d", kept)
		}
	})
	t.Run("访问地址不在候选内时保留原始候选", func(t *testing.T) {
		sdp, kept := pruneAnswerCandidates(testAnswerSDP, []string{"203.0.113.10"})
		if kept != 0 || sdp != testAnswerSDP {
			t.Fatalf("kept=%d", kept)
		}
	})
	t.Run("IPv4-mapped IPv6 归一化后匹配", func(t *testing.T) {
		_, kept := pruneAnswerCandidates(testAnswerSDP, []string{"::ffff:192.168.3.30"})
		if kept != 1 {
			t.Fatalf("kept=%d", kept)
		}
	})
	t.Run("空允许列表不裁剪", func(t *testing.T) {
		sdp, kept := pruneAnswerCandidates(testAnswerSDP, nil)
		if kept != 0 || sdp != testAnswerSDP {
			t.Fatalf("kept=%d", kept)
		}
	})
}

func TestResolveAccessHosts(t *testing.T) {
	cases := []struct {
		name string
		host string
		want []string
	}{
		{name: "带端口的 IPv4 Host", host: "192.168.3.30:7576", want: []string{"192.168.3.30"}},
		{name: "不带端口的 IPv4", host: "192.168.3.30", want: []string{"192.168.3.30"}},
		{name: "方括号 IPv6 Host", host: "[240e:399:ea2:a970:566d:26cd:f684:8185]:7576", want: []string{"240e:399:ea2:a970:566d:26cd:f684:8185"}},
		{name: "IPv4-mapped 归一化", host: "::ffff:192.168.3.30", want: []string{"192.168.3.30"}},
		{name: "空 Host", host: "", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAccessHosts(context.Background(), tc.host)
			if len(got) != len(tc.want) {
				t.Fatalf("resolveAccessHosts(%q) = %v, want %v", tc.host, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("resolveAccessHosts(%q) = %v, want %v", tc.host, got, tc.want)
				}
			}
		})
	}
}
