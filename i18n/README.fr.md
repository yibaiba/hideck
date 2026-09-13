<div align="center">

# HiDeck

**Console auto-hébergée pour modems Qualcomm 4G / LTE / 5G**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md) · **Français** · [Deutsch](README.de.md) · [Português](README.pt-BR.md) · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

Gérez les modems USB, le proxy cellulaire, les SMS, le WiFi calling / la voix IMS, l’eSIM et les tâches planifiées depuis le navigateur.

| | |
| --- | --- |
| Interface web | `http://YOUR_IP:7575` |
| Identifiants par défaut | `admin` / `admin` (à changer à la première connexion) |
| Base de données | SQLite (`data/hideck.db`) |

## Fonctions

- **Appareils** — découverte USB ; QMI, MBIM, AT, PC/SC ; radio et identité SIM en direct
- **Proxy** — SOCKS5 / HTTP lié au modem avec `SO_BINDTODEVICE`
- **SMS** — boîte de réception, envoi, contacts ; vcf/csv iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Téléphone** — WiFi calling, IMS logiciel cellulaire ou VoLTE natif ; mise en attente, appel en attente, décroché écoute seule
- **VoWiFi** — SWu / IMS, enregistrements ; pool de proxies par pays, nœud figé ou accès direct
- **VoLTE natif** — IMS du modem (`phone_mode=volte`), pas d’ePDG ; mapping MBN unique Chine uniquement
- **eSIM** — télécharger, activer, désactiver, renommer, supprimer ; code d’activation ou QR / PDF
- **Automatisation et notifications** — tâches planifiées ; Telegram, e-mail, Bark, Feishu, WeCom, WeChat, QQ

Notes de protocole : [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [opérateurs](../docs/operator-notes.md) · [matériel](../docs/modem-hardware.md)

## Démarrage rapide

Docker (recommandé). Il faut Linux, curl, Docker Compose, le réseau host et l’accès USB. L’image embarque les bibliothèques AMR/MP3 pour l’enregistrement d’appels.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Ouvrez `http://YOUR_IP:7575`.

Avec un nom d’hôte public (DNS A/AAAA vers la machine, `443/TCP` et `443/UDP` ouverts) :

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Image : `yibaiba/hideck:latest`. Compose utilise `network_mode: host`, `privileged: true`, `/dev`, et persiste `config/`, `data/`, `logs/`. Voir [DOCKERHUB.md](../DOCKERHUB.md) et [HTTPS / WebRTC](../docs/https-webrtc.md).

```bash
docker compose ps
docker compose logs -f hideck
```

## Captures

![Connexion](../docs/images/login.jpg)

![Tableau de bord](../docs/images/dashboard.jpg)

| Appareils | Téléphone |
| --- | --- |
| ![Appareils](../docs/images/devices.jpg) | ![Téléphone](../docs/images/phone.jpg) |

| Commandes | Proxy |
| --- | --- |
| ![Commandes](../docs/images/commands.jpg) | ![Proxy](../docs/images/console-proxy.jpg) |

## Installation binaire

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| Fichier | Plateforme |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64 bits, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | ARM 32 bits, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl statique, sans UPX |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt ARM 32 bits |

Sur OpenWrt, n’utilisez que `openwrt_*`. Voir [packaging/openwrt/README.md](../packaging/openwrt/README.md).

## Configuration

Copiez [config/config.example.yaml](../config/config.example.yaml) vers `config/config.yaml`.

| Clé | Défaut | Notes |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | HTTPS intégré ; laisser désactivé derrière un reverse proxy |
| `server.https_port` | `7576` | Port HTTPS intégré |
| `server.webrtc_udp_address` | `:7580` | UDP audio WebRTC |
| `server.webrtc_public_host` | vide | Hôte ICE public derrière NAT |
| `web.username` / `web.password` | `admin` | À changer tout de suite |
| `vowifi.enabled` | `false` | VoWiFi global |
| `system.openwrt_dynamic_interfaces` | `false` | Mapping netifd OpenWrt |

Ne commitez pas de secrets. Les champs SIM PIN ne stockent que des *noms* de variables d’environnement (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` écrase le fichier.

## Compilation

Go `1.26.4+`, Node.js / npm. `make build-*` nécessite UPX.

```bash
make build-amd64    # aussi : build-arm64, build-armv7, build-all
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

Proxy de dev : `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` dans `web/.env.local`. API : `/api/docs`.

## Licence

Usage personnel d’apprentissage, de recherche et de test. Ce n’est pas une stack téléphonie de production. Indépendant de Quectel, Qualcomm et des opérateurs. Respectez le droit local et les conditions de votre opérateur.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go` est AGPL-3.0. Lisez [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) avant de distribuer des binaires ou des images.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
