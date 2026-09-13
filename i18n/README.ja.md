<div align="center">

# HiDeck

**Qualcomm 4G / LTE / 5G モデム向けセルフホストコンソール**

[English](../README.md) · [简体中文](README.zh-CN.md) · **日本語** · [한국어](README.ko.md) · [Español](README.es.md) · [Français](README.fr.md) · [Deutsch](README.de.md) · [Português](README.pt-BR.md) · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

USB モデム、セルラープロキシ、SMS、WiFi calling / IMS 音声、eSIM、スケジュールタスクをブラウザから管理します。

| | |
| --- | --- |
| Web UI | `http://YOUR_IP:7575` |
| 初期ログイン | `admin` / `admin`（初回サインイン時に変更） |
| データベース | SQLite（`data/hideck.db`） |

## 機能

- **デバイス** — USB 自動検出。QMI、MBIM、AT、PC/SC。電波と SIM 識別をリアルタイム表示
- **プロキシ** — モデムに `SO_BINDTODEVICE` でバインドした SOCKS5 / HTTP
- **SMS** — 受信、送信、連絡先。iOS / Google / Samsung / Xiaomi / Huawei / OPPO / vivo の vcf / csv
- **電話** — WiFi calling、セルラーソフトウェア IMS、ネイティブ VoLTE。保留、キャッチホン、聞き取り専用応答
- **VoWiFi** — SWu / IMS、録音。国別プロキシプール、ノード固定、または直結
- **ネイティブ VoLTE** — モデム IMS（`phone_mode=volte`）、ePDG なし。中国向けの一意な MBN マッピングのみ
- **eSIM** — ダウンロード、有効化、無効化、名前変更、削除。アクティベーションコードまたは QR / PDF
- **自動化と通知** — スケジュールタスク。Telegram、メール、Bark、Feishu、WeCom、WeChat、QQ

プロトコル：[VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [事業者](../docs/operator-notes.md) · [ハードウェア](../docs/modem-hardware.md)

## クイックスタート

Docker を推奨します。Linux、curl、Docker Compose、ホストネットワーク、USB アクセスが必要です。イメージには通話録音用の AMR/MP3 ライブラリが含まれます。

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

`http://YOUR_IP:7575` を開きます。

公開ホスト名（DNS の A/AAAA をホストへ、`443/TCP` と `443/UDP` を開放）：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

イメージ：`yibaiba/hideck:latest`。Compose は `network_mode: host`、`privileged: true`、`/dev` を使い、`config/`、`data/`、`logs/` を永続化します。[DOCKERHUB.md](../DOCKERHUB.md) と [HTTPS / WebRTC](../docs/https-webrtc.md) を参照してください。

```bash
docker compose ps
docker compose logs -f hideck
```

## スクリーンショット

![ログイン](../docs/images/login.jpg)

![ダッシュボード](../docs/images/dashboard.jpg)

| デバイス | 電話 |
| --- | --- |
| ![デバイス](../docs/images/devices.jpg) | ![電話](../docs/images/phone.jpg) |

| コマンド | プロキシ |
| --- | --- |
| ![コマンド](../docs/images/commands.jpg) | ![プロキシ](../docs/images/console-proxy.jpg) |

## バイナリインストール

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| ファイル | 対象 |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64、glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit、glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | 32-bit ARM、glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64、musl 静的、UPX なし |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt 32-bit ARM |

OpenWrt では `openwrt_*` のみを使ってください。[packaging/openwrt/README.md](../packaging/openwrt/README.md) を参照。

## 設定

[config/config.example.yaml](../config/config.example.yaml) を `config/config.yaml` にコピーします。

| キー | デフォルト | 説明 |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | 内蔵 HTTPS。リバースプロキシ配下ではオフ |
| `server.https_port` | `7576` | 内蔵 HTTPS ポート |
| `server.webrtc_udp_address` | `:7580` | WebRTC 音声 UDP |
| `server.webrtc_public_host` | 空 | NAT 配下の公開 ICE ホスト |
| `web.username` / `web.password` | `admin` | すぐに変更する |
| `vowifi.enabled` | `false` | グローバル VoWiFi |
| `system.openwrt_dynamic_interfaces` | `false` | OpenWrt netifd マッピング |

秘密情報をコミットしないでください。SIM PIN 欄は環境変数名だけを保存します（`HIDECK_SIM_PIN_READER1`）。`PROXY_WEB_PASSWORD` はファイルより優先されます。

## ビルド

Go `1.26.4+`、Node.js / npm。`make build-*` には UPX が必要です。

```bash
make build-amd64    # ほか: build-arm64, build-armv7, build-all
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

開発用プロキシ：`web/.env.local` に `VITE_API_PROXY_TARGET=http://127.0.0.1:7575`。API ドキュメント：`/api/docs`。

## ライセンス

個人の学習、研究、テスト向けです。本番の電話基盤ではありません。Quectel、Qualcomm、通信事業者とは無関係です。現地の法令と契約に従ってください。

[PolyForm Noncommercial 1.0.0](../LICENSE)。`third_party/vowifi-go` は AGPL-3.0 です。バイナリやイメージを配布する前に [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) を読んでください。

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
