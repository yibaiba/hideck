<div align="center">

# HiDeck

**Self-hosted console for Qualcomm 4G / LTE / 5G modems**

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

Manage USB modems, cellular proxy, SMS, WiFi calling / IMS voice, eSIM, and scheduled tasks from a browser.

| | |
| --- | --- |
| Web UI | `http://YOUR_IP:7575` |
| Default login | `admin` / `admin` (change on first sign-in) |
| Database | SQLite (`data/hideck.db`) |

## Features

- **Devices** — USB auto-discovery; QMI, MBIM, AT, PC/SC; live radio and SIM identity
- **Proxy** — SOCKS5 / HTTP bound to the modem with `SO_BINDTODEVICE`
- **SMS** — inbox, send, contacts; vcf/csv from iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Phone** — WiFi calling, cellular software IMS, or native VoLTE; hold, call waiting, listen-only answer
- **VoWiFi** — SWu / IMS, recordings; country proxy pool, pin a node, or go direct
- **Native VoLTE** — modem IMS (`phone_mode=volte`), no ePDG; unique China MBN mapping only
- **eSIM** — download, enable, disable, rename, delete; activation code or QR / PDF
- **Automation & notify** — scheduled tasks; Telegram, email, Bark, Feishu, WeCom, WeChat, QQ

Protocol notes: [VoWiFi](docs/vowifi-protocol-alignment.md) · [VoLTE](docs/volte-native.md) · [operators](docs/operator-notes.md) · [hardware](docs/modem-hardware.md) · [module voice troubleshooting](docs/modem-voice-troubleshooting.md)

Outgoing call/SMS quotas are per SIM and survive restarts: [limits and configuration](docs/outbound-limits.md).

## Quick start

Docker (recommended). Needs Linux, curl, Docker Compose, host networking, and USB access. The image includes ADB, ALSA tools, AMR/MP3 libraries, and the embedded module-side voice runtime. The host still needs a USB audio driver; see [module voice prerequisites](DOCKERHUB.md#module-voice-dependencies).

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Then open `http://YOUR_IP:7575`.

With a public hostname (DNS A/AAAA to the host, open `443/TCP` and `443/UDP`):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Image: `yibaiba/hideck:latest`. Compose uses `network_mode: host`, `privileged: true`, `/dev`, and persists `config/`, `data/`, `logs/`. See [DOCKERHUB.md](DOCKERHUB.md) and [HTTPS / WebRTC](docs/https-webrtc.md).

For Feiniu fnOS, the optional [FPK packaging](packaging/fnos/README.md) reuses the Docker image with an application entry and persistent app directories; on-device validation is still required.

```bash
docker compose ps
docker compose logs -f hideck
```

For PC/SC USB smart-card readers, also configure the host service and optional
Compose override described in [Docker PC/SC setup](DOCKERHUB.md#pcsc-smart-card-readers).

## Screenshots

![Login](docs/images/login.jpg)

![Dashboard](docs/images/dashboard.jpg)

| Devices | Phone |
| --- | --- |
| ![Devices](docs/images/devices.jpg) | ![Phone](docs/images/phone.jpg) |

| Commands | Proxy |
| --- | --- |
| ![Commands](docs/images/commands.jpg) | ![Proxy](docs/images/console-proxy.jpg) |

## Binary install

The installer also installs ADB, ALSA tools and recording libraries, then checks ADB capabilities and host USB audio support. Missing dependencies produce a nonzero exit status even if the HiDeck service was installed; they are not reported as a fully successful deployment. These checks do not connect to or reboot a modem.

OpenWrt keeps module voice optional: use the separate `hideck-adb` and `hideck-modem-voice` packages. See [OpenWrt packaging](packaging/openwrt/README.md) for IPK/APK installation and target matching.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.23 \
  HIDECK_ARCH=linux_amd64 sh
```

| Asset | Platform |
| --- | --- |
| `hideck_v2.1.23_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.23_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit, glibc + UPX |
| `hideck_v2.1.23_linux_armv7` | 32-bit ARM, glibc + UPX |
| `hideck_v2.1.23_openwrt_amd64` | OpenWrt x86_64, musl static, no UPX |
| `hideck_v2.1.23_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.23_openwrt_armv7` | OpenWrt 32-bit ARM |
| `hideck_v2.1.23_openwrt_dynamic_amd64` | OpenWrt x86_64, dynamic musl, MP3 encoding capable |
| `hideck_v2.1.23_openwrt_dynamic_arm64` | OpenWrt aarch64, dynamic musl, MP3 encoding capable |
| `hideck_v2.1.23_openwrt_dynamic_armv7` | OpenWrt 32-bit ARM, dynamic musl, MP3 encoding capable |

On OpenWrt, use `openwrt_*` only. The package-manager archives and binary installer use the static assets. The dynamic assets require matching runtime libraries and are intended for MP3 call recording; see [OpenWrt packaging](packaging/openwrt/README.md#openwrt-dynamic-musl).

## Configuration

Copy [config/config.example.yaml](config/config.example.yaml) to `config/config.yaml`.

| Key | Default | Notes |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | Built-in HTTPS; keep off behind a reverse proxy |
| `server.https_port` | `7576` | Built-in HTTPS port |
| `server.webrtc_udp_address` | `:7580` | WebRTC audio UDP |
| `server.webrtc_public_host` | empty | Public ICE host behind NAT |
| `web.username` / `web.password` | `admin` | Change immediately |
| `vowifi.enabled` | `false` | Global VoWiFi |
| `system.openwrt_dynamic_interfaces` | `false` | OpenWrt netifd mapping |

Do not commit secrets. SIM PIN fields store environment variable *names* only (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` overrides the file.

## Build

Go `1.26.4+`, Node.js / npm. `make build-*` requires UPX.

```bash
make build-amd64    # also: build-arm64, build-armv7, build-all
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

Dev proxy: `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` in `web/.env.local`. API docs: `/api/docs`.

## License

Personal learning, research, and testing. Not a production telephony stack. Independent of Quectel, Qualcomm, and carriers. Follow local law and your operator’s terms.

[PolyForm Noncommercial 1.0.0](LICENSE). `third_party/vowifi-go` is AGPL-3.0. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) before distributing binaries or images.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
