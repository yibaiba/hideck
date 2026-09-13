<div align="center">

# HiDeck

**Self-hosted консоль для модемов Qualcomm 4G / LTE / 5G**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md) · [Français](README.fr.md) · [Deutsch](README.de.md) · [Português](README.pt-BR.md) · **Русский**

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

Управляйте USB-модемами, сотовым прокси, SMS, WiFi calling / IMS-голосом, eSIM и заданиями по расписанию из браузера.

| | |
| --- | --- |
| Веб-интерфейс | `http://YOUR_IP:7575` |
| Логин по умолчанию | `admin` / `admin` (смените при первом входе) |
| База данных | SQLite (`data/hideck.db`) |

## Возможности

- **Устройства** — автообнаружение USB; QMI, MBIM, AT, PC/SC; радио и идентификатор SIM в реальном времени
- **Прокси** — SOCKS5 / HTTP, привязанный к модему через `SO_BINDTODEVICE`
- **SMS** — входящие, отправка, контакты; vcf/csv из iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Телефон** — WiFi calling, сотовый software IMS или нативный VoLTE; удержание, ожидание вызова, ответ только на прослушивание
- **VoWiFi** — SWu / IMS, записи; пул прокси по странам, закрепить узел или идти напрямую
- **Нативный VoLTE** — IMS модема (`phone_mode=volte`), без ePDG; только уникальный китайский MBN-mapping
- **eSIM** — скачать, включить, выключить, переименовать, удалить; код активации или QR / PDF
- **Автоматизация и уведомления** — задания по расписанию; Telegram, почта, Bark, Feishu, WeCom, WeChat, QQ

Заметки по протоколам: [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [операторы](../docs/operator-notes.md) · [оборудование](../docs/modem-hardware.md)

## Быстрый старт

Docker (рекомендуется). Нужны Linux, curl, Docker Compose, host-сеть и доступ к USB. В образ входят библиотеки AMR/MP3 для записи звонков.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Откройте `http://YOUR_IP:7575`.

С публичным именем хоста (DNS A/AAAA на машину, открыты `443/TCP` и `443/UDP`):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Образ: `yibaiba/hideck:latest`. Compose использует `network_mode: host`, `privileged: true`, `/dev` и сохраняет `config/`, `data/`, `logs/`. См. [DOCKERHUB.md](../DOCKERHUB.md) и [HTTPS / WebRTC](../docs/https-webrtc.md).

```bash
docker compose ps
docker compose logs -f hideck
```

## Скриншоты

![Вход](../docs/images/login.jpg)

![Панель](../docs/images/dashboard.jpg)

| Устройства | Телефон |
| --- | --- |
| ![Устройства](../docs/images/devices.jpg) | ![Телефон](../docs/images/phone.jpg) |

| Команды | Прокси |
| --- | --- |
| ![Команды](../docs/images/commands.jpg) | ![Прокси](../docs/images/console-proxy.jpg) |

## Установка бинарника

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| Файл | Платформа |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | 32-bit ARM, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl static, без UPX |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt 32-bit ARM |

На OpenWrt используйте только `openwrt_*`. См. [packaging/openwrt/README.md](../packaging/openwrt/README.md).

## Конфигурация

Скопируйте [config/config.example.yaml](../config/config.example.yaml) в `config/config.yaml`.

| Ключ | По умолчанию | Примечание |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | Встроенный HTTPS; держите выключенным за reverse proxy |
| `server.https_port` | `7576` | Порт встроенного HTTPS |
| `server.webrtc_udp_address` | `:7580` | UDP аудио WebRTC |
| `server.webrtc_public_host` | пусто | Публичный ICE-хост за NAT |
| `web.username` / `web.password` | `admin` | Смените сразу |
| `vowifi.enabled` | `false` | Глобальный VoWiFi |
| `system.openwrt_dynamic_interfaces` | `false` | Маппинг netifd на OpenWrt |

Не коммитьте секреты. Поля SIM PIN хранят только *имена* переменных окружения (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` перекрывает файл.

## Сборка

Go `1.26.4+`, Node.js / npm. Для `make build-*` нужен UPX.

```bash
make build-amd64    # также: build-arm64, build-armv7, build-all
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

Dev-прокси: `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` в `web/.env.local`. Документация API: `/api/docs`.

## Лицензия

Для личного обучения, исследований и тестов. Это не производственный телефонный стек. Не связан с Quectel, Qualcomm и операторами. Соблюдайте местное право и условия своего оператора.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go` — AGPL-3.0. Перед распространением бинарников или образов прочитайте [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md).

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
