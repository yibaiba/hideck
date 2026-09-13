<div align="center">

# HiDeck

**Consola autoalojada para módems Qualcomm 4G / LTE / 5G**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · **Español** · [Français](README.fr.md) · [Deutsch](README.de.md) · [Português](README.pt-BR.md) · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

Gestiona módems USB, proxy celular, SMS, WiFi calling / voz IMS, eSIM y tareas programadas desde el navegador.

| | |
| --- | --- |
| Interfaz web | `http://YOUR_IP:7575` |
| Acceso por defecto | `admin` / `admin` (cámbialo en el primer inicio de sesión) |
| Base de datos | SQLite (`data/hideck.db`) |

## Funciones

- **Dispositivos** — descubrimiento USB; QMI, MBIM, AT, PC/SC; radio y SIM en tiempo real
- **Proxy** — SOCKS5 / HTTP ligado al módem con `SO_BINDTODEVICE`
- **SMS** — bandeja, envío, contactos; vcf/csv de iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Teléfono** — WiFi calling, IMS por software celular o VoLTE nativo; espera, llamada en espera, respuesta solo-escucha
- **VoWiFi** — SWu / IMS, grabaciones; pool de proxies por país, fijar un nodo o ir directo
- **VoLTE nativo** — IMS del módem (`phone_mode=volte`), sin ePDG; solo el mapeo MBN único de China
- **eSIM** — descargar, activar, desactivar, renombrar, borrar; código de activación o QR / PDF
- **Automatización y avisos** — tareas programadas; Telegram, correo, Bark, Feishu, WeCom, WeChat, QQ

Notas de protocolo: [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [operadores](../docs/operator-notes.md) · [hardware](../docs/modem-hardware.md)

## Inicio rápido

Docker (recomendado). Hace falta Linux, curl, Docker Compose, red host y acceso USB. La imagen incluye librerías AMR/MP3 para grabar llamadas.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Abre `http://YOUR_IP:7575`.

Con un nombre público (DNS A/AAAA al host, puertos `443/TCP` y `443/UDP` abiertos):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Imagen: `yibaiba/hideck:latest`. Compose usa `network_mode: host`, `privileged: true`, `/dev`, y persiste `config/`, `data/`, `logs/`. Ver [DOCKERHUB.md](../DOCKERHUB.md) y [HTTPS / WebRTC](../docs/https-webrtc.md).

```bash
docker compose ps
docker compose logs -f hideck
```

## Capturas

![Inicio de sesión](../docs/images/login.jpg)

![Panel](../docs/images/dashboard.jpg)

| Dispositivos | Teléfono |
| --- | --- |
| ![Dispositivos](../docs/images/devices.jpg) | ![Teléfono](../docs/images/phone.jpg) |

| Comandos | Proxy |
| --- | --- |
| ![Comandos](../docs/images/commands.jpg) | ![Proxy](../docs/images/console-proxy.jpg) |

## Instalación binaria

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| Archivo | Plataforma |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | ARM 32-bit, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl estático, sin UPX |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt ARM 32-bit |

En OpenWrt usa solo `openwrt_*`. Ver [packaging/openwrt/README.md](../packaging/openwrt/README.md).

## Configuración

Copia [config/config.example.yaml](../config/config.example.yaml) a `config/config.yaml`.

| Clave | Por defecto | Notas |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | HTTPS integrado; déjalo apagado detrás de un proxy inverso |
| `server.https_port` | `7576` | Puerto HTTPS integrado |
| `server.webrtc_udp_address` | `:7580` | UDP de audio WebRTC |
| `server.webrtc_public_host` | vacío | Host ICE público detrás de NAT |
| `web.username` / `web.password` | `admin` | Cámbialo de inmediato |
| `vowifi.enabled` | `false` | VoWiFi global |
| `system.openwrt_dynamic_interfaces` | `false` | Mapeo netifd de OpenWrt |

No subas secretos. Los campos SIM PIN guardan solo *nombres* de variables de entorno (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` pisa el archivo.

## Compilación

Go `1.26.4+`, Node.js / npm. `make build-*` necesita UPX.

```bash
make build-amd64    # también: build-arm64, build-armv7, build-all
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

Proxy de desarrollo: `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` en `web/.env.local`. API: `/api/docs`.

## Licencia

Aprendizaje, investigación y pruebas personales. No es una centralita de producción. Independiente de Quectel, Qualcomm y los operadores. Cumple la ley local y las condiciones de tu operador.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go` es AGPL-3.0. Lee [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) antes de distribuir binarios o imágenes.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
