<div align="center">

# HiDeck

**Selbst gehostete Konsole für Qualcomm-4G-/LTE-/5G-Modems**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md) · [Français](README.fr.md) · **Deutsch** · [Português](README.pt-BR.md) · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

USB-Modems, Mobilfunk-Proxy, SMS, WiFi calling / IMS-Sprache, eSIM und geplante Aufgaben im Browser verwalten.

| | |
| --- | --- |
| Web-UI | `http://YOUR_IP:7575` |
| Standard-Login | `admin` / `admin` (beim ersten Anmelden ändern) |
| Datenbank | SQLite (`data/hideck.db`) |

## Funktionen

- **Geräte** — USB-Autodiscovery; QMI, MBIM, AT, PC/SC; Funk und SIM-Identität live
- **Proxy** — SOCKS5 / HTTP, mit `SO_BINDTODEVICE` an das Modem gebunden
- **SMS** — Posteingang, Senden, Kontakte; vcf/csv von iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Telefon** — WiFi calling, zellulares Software-IMS oder natives VoLTE; Halten, Anklopfen, Nur-Hören-Annahme
- **VoWiFi** — SWu / IMS, Aufnahmen; Proxy-Pool pro Land, Knoten pinnen oder direkt
- **Natives VoLTE** — Modem-IMS (`phone_mode=volte`), ohne ePDG; nur das eindeutige China-MBN-Mapping
- **eSIM** — herunterladen, aktivieren, deaktivieren, umbenennen, löschen; Aktivierungscode oder QR / PDF
- **Automatisierung und Benachrichtigung** — geplante Aufgaben; Telegram, E-Mail, Bark, Feishu, WeCom, WeChat, QQ

Protokollnotizen: [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [Betreiber](../docs/operator-notes.md) · [Hardware](../docs/modem-hardware.md)

## Schnellstart

Docker (empfohlen). Braucht Linux, curl, Docker Compose, Host-Netzwerk und USB-Zugriff. Das Image enthält AMR/MP3-Bibliotheken für Gesprächsaufzeichnung.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Danach `http://YOUR_IP:7575` öffnen.

Mit öffentlichem Hostnamen (DNS A/AAAA auf den Host, `443/TCP` und `443/UDP` offen):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Image: `yibaiba/hideck:latest`. Compose nutzt `network_mode: host`, `privileged: true`, `/dev` und persistiert `config/`, `data/`, `logs/`. Siehe [DOCKERHUB.md](../DOCKERHUB.md) und [HTTPS / WebRTC](../docs/https-webrtc.md).

```bash
docker compose ps
docker compose logs -f hideck
```

## Screenshots

![Anmeldung](../docs/images/login.jpg)

![Dashboard](../docs/images/dashboard.jpg)

| Geräte | Telefon |
| --- | --- |
| ![Geräte](../docs/images/devices.jpg) | ![Telefon](../docs/images/phone.jpg) |

| Befehle | Proxy |
| --- | --- |
| ![Befehle](../docs/images/commands.jpg) | ![Proxy](../docs/images/console-proxy.jpg) |

## Binärinstallation

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| Datei | Plattform |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-Bit, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | 32-Bit-ARM, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl statisch, ohne UPX |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt 32-Bit-ARM |

Unter OpenWrt nur `openwrt_*` verwenden. Siehe [packaging/openwrt/README.md](../packaging/openwrt/README.md).

## Konfiguration

[config/config.example.yaml](../config/config.example.yaml) nach `config/config.yaml` kopieren.

| Schlüssel | Standard | Hinweis |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | Eingebautes HTTPS; hinter Reverse-Proxy aus lassen |
| `server.https_port` | `7576` | Port für eingebautes HTTPS |
| `server.webrtc_udp_address` | `:7580` | WebRTC-Audio-UDP |
| `server.webrtc_public_host` | leer | Öffentlicher ICE-Host hinter NAT |
| `web.username` / `web.password` | `admin` | Sofort ändern |
| `vowifi.enabled` | `false` | Globales VoWiFi |
| `system.openwrt_dynamic_interfaces` | `false` | OpenWrt-netifd-Mapping |

Keine Geheimnisse committen. SIM-PIN-Felder speichern nur Umgebungsvariablen-*Namen* (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` überschreibt die Datei.

## Bauen

Go `1.26.4+`, Node.js / npm. `make build-*` braucht UPX.

```bash
make build-amd64    # außerdem: build-arm64, build-armv7, build-all
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

Dev-Proxy: `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` in `web/.env.local`. API-Doku: `/api/docs`.

## Lizenz

Zum persönlichen Lernen, Forschen und Testen. Kein Produktions-Telefonie-Stack. Unabhängig von Quectel, Qualcomm und Netzbetreibern. Lokales Recht und die AGB des Betreibers einhalten.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go` ist AGPL-3.0. Vor der Verteilung von Binaries oder Images [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) lesen.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
