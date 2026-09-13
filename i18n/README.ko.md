<div align="center">

# HiDeck

**Qualcomm 4G / LTE / 5G 모뎀용 셀프 호스팅 콘솔**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · **한국어** · [Español](README.es.md) · [Français](README.fr.md) · [Deutsch](README.de.md) · [Português](README.pt-BR.md) · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

브라우저에서 USB 모뎀, 셀룰러 프록시, SMS, WiFi calling / IMS 음성, eSIM, 예약 작업을 관리합니다.

| | |
| --- | --- |
| Web UI | `http://YOUR_IP:7575` |
| 기본 로그인 | `admin` / `admin` (첫 로그인 시 변경) |
| 데이터베이스 | SQLite (`data/hideck.db`) |

## 기능

- **장치** — USB 자동 검색. QMI, MBIM, AT, PC/SC. 무선과 SIM 식별 실시간 표시
- **프록시** — `SO_BINDTODEVICE`로 모뎀에 바인딩한 SOCKS5 / HTTP
- **SMS** — 수신, 발신, 연락처. iOS / Google / Samsung / Xiaomi / Huawei / OPPO / vivo의 vcf / csv
- **전화** — WiFi calling, 셀룰러 소프트웨어 IMS, 네이티브 VoLTE. 보류, 통화 대기, 듣기 전용 응답
- **VoWiFi** — SWu / IMS, 녹음. 국가별 프록시 풀, 노드 고정, 또는 직접 연결
- **네이티브 VoLTE** — 모뎀 IMS (`phone_mode=volte`), ePDG 없음. 중국용 고유 MBN 매핑만
- **eSIM** — 다운로드, 활성화, 비활성화, 이름 변경, 삭제. 활성화 코드 또는 QR / PDF
- **자동화와 알림** — 예약 작업. Telegram, 이메일, Bark, Feishu, WeCom, WeChat, QQ

프로토콜: [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [통신사](../docs/operator-notes.md) · [하드웨어](../docs/modem-hardware.md)

## 빠른 시작

Docker를 권장합니다. Linux, curl, Docker Compose, 호스트 네트워크, USB 접근이 필요합니다. 이미지에 통화 녹음용 AMR/MP3 라이브러리가 포함됩니다.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

`http://YOUR_IP:7575` 을 엽니다.

공인 호스트 이름 (DNS A/AAAA를 호스트로, `443/TCP`와 `443/UDP` 개방):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

이미지: `yibaiba/hideck:latest`. Compose는 `network_mode: host`, `privileged: true`, `/dev`를 사용하고 `config/`, `data/`, `logs/`를 유지합니다. [DOCKERHUB.md](../DOCKERHUB.md)와 [HTTPS / WebRTC](../docs/https-webrtc.md)를 참고하세요.

```bash
docker compose ps
docker compose logs -f hideck
```

## 스크린샷

![로그인](../docs/images/login.jpg)

![대시보드](../docs/images/dashboard.jpg)

| 장치 | 전화 |
| --- | --- |
| ![장치](../docs/images/devices.jpg) | ![전화](../docs/images/phone.jpg) |

| 명령 | 프록시 |
| --- | --- |
| ![명령](../docs/images/commands.jpg) | ![프록시](../docs/images/console-proxy.jpg) |

## 바이너리 설치

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| 파일 | 플랫폼 |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | 32-bit ARM, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl 정적, UPX 없음 |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt 32-bit ARM |

OpenWrt에서는 `openwrt_*`만 사용하세요. [packaging/openwrt/README.md](../packaging/openwrt/README.md)를 참고하세요.

## 설정

[config/config.example.yaml](../config/config.example.yaml)을 `config/config.yaml`로 복사합니다.

| 키 | 기본값 | 설명 |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | 내장 HTTPS. 리버스 프록시 뒤에서는 끄기 |
| `server.https_port` | `7576` | 내장 HTTPS 포트 |
| `server.webrtc_udp_address` | `:7580` | WebRTC 오디오 UDP |
| `server.webrtc_public_host` | 비움 | NAT 뒤 공인 ICE 호스트 |
| `web.username` / `web.password` | `admin` | 즉시 변경 |
| `vowifi.enabled` | `false` | 전역 VoWiFi |
| `system.openwrt_dynamic_interfaces` | `false` | OpenWrt netifd 매핑 |

비밀을 커밋하지 마세요. SIM PIN 필드는 환경 변수 이름만 저장합니다 (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD`가 파일보다 우선합니다.

## 빌드

Go `1.26.4+`, Node.js / npm. `make build-*`에는 UPX가 필요합니다.

```bash
make build-amd64    # 그 외: build-arm64, build-armv7, build-all
```

```bash
npm ci --prefix web && npm run build --prefix web
cp -R web/dist internal/web/dist
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -tags "with_utls nomsgpack" -o dist/hideck_linux_amd64 ./cmd/hideck
```

```bash
npm test --prefix web && npm run typecheck --prefix web
go test -timeout=60s ./cmd/... ./internal/... ./pkg/...
```

개발 프록시: `web/.env.local`에 `VITE_API_PROXY_TARGET=http://127.0.0.1:7575`. API 문서: `/api/docs`.

## 라이선스

개인 학습, 연구, 테스트용입니다. 상용 전화 스택이 아닙니다. Quectel, Qualcomm, 통신사와 무관합니다. 현지 법률과 통신사 약관을 지키세요.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go`는 AGPL-3.0입니다. 바이너리나 이미지를 배포하기 전에 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)를 읽으세요.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
