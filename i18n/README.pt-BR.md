<div align="center">

# HiDeck

**Console auto-hospedada para modems Qualcomm 4G / LTE / 5G**

[English](../README.md) · [简体中文](README.zh-CN.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md) · [Français](README.fr.md) · [Deutsch](README.de.md) · **Português** · [Русский](README.ru.md)

<p>
  <a href="https://github.com/yibaiba/hideck/releases/latest"><img src="https://img.shields.io/github/v/release/yibaiba/hideck" alt="Release"></a>
  <a href="https://hub.docker.com/r/yibaiba/hideck"><img src="https://img.shields.io/badge/docker-yibaiba%2Fhideck-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
  <a href="../go.mod"><img src="https://img.shields.io/badge/Go-1.26.4%2B-00ADD8?logo=go" alt="Go"></a>
  <a href="../web/package.json"><img src="https://img.shields.io/badge/Vue-3-42b883?logo=vue.js" alt="Vue 3"></a>
  <a href="../LICENSE"><img src="https://img.shields.io/static/v1?label=License&message=PolyForm%20NC%201.0.0&color=blue" alt="License: PolyForm Noncommercial 1.0.0"></a>
</p>

</div>

Gerencie modems USB, proxy celular, SMS, WiFi calling / voz IMS, eSIM e tarefas agendadas pelo navegador.

| | |
| --- | --- |
| Interface web | `http://YOUR_IP:7575` |
| Login padrão | `admin` / `admin` (altere no primeiro acesso) |
| Banco de dados | SQLite (`data/hideck.db`) |

## Recursos

- **Dispositivos** — descoberta USB; QMI, MBIM, AT, PC/SC; rádio e identidade do SIM ao vivo
- **Proxy** — SOCKS5 / HTTP ligado ao modem com `SO_BINDTODEVICE`
- **SMS** — caixa de entrada, envio, contatos; vcf/csv do iOS, Google, Samsung, Xiaomi, Huawei, OPPO, vivo
- **Telefone** — WiFi calling, IMS por software celular ou VoLTE nativo; espera, chamada em espera, atender só-ouvir
- **VoWiFi** — SWu / IMS, gravações; pool de proxies por país, fixar um nó ou ir direto
- **VoLTE nativo** — IMS do modem (`phone_mode=volte`), sem ePDG; só o mapeamento MBN único da China
- **eSIM** — baixar, ativar, desativar, renomear, apagar; código de ativação ou QR / PDF
- **Automação e avisos** — tarefas agendadas; Telegram, e-mail, Bark, Feishu, WeCom, WeChat, QQ

Notas de protocolo: [VoWiFi](../docs/vowifi-protocol-alignment.md) · [VoLTE](../docs/volte-native.md) · [operadoras](../docs/operator-notes.md) · [hardware](../docs/modem-hardware.md)

## Início rápido

Docker (recomendado). Precisa de Linux, curl, Docker Compose, rede host e acesso USB. A imagem inclui bibliotecas AMR/MP3 para gravação de chamadas.

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

Abra `http://YOUR_IP:7575`.

Com um hostname público (DNS A/AAAA no host, `443/TCP` e `443/UDP` abertos):

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

Imagem: `yibaiba/hideck:latest`. O Compose usa `network_mode: host`, `privileged: true`, `/dev`, e persiste `config/`, `data/`, `logs/`. Veja [DOCKERHUB.md](../DOCKERHUB.md) e [HTTPS / WebRTC](../docs/https-webrtc.md).

```bash
docker compose ps
docker compose logs -f hideck
```

## Capturas

![Login](../docs/images/login.jpg)

![Painel](../docs/images/dashboard.jpg)

| Dispositivos | Telefone |
| --- | --- |
| ![Dispositivos](../docs/images/devices.jpg) | ![Telefone](../docs/images/phone.jpg) |

| Comandos | Proxy |
| --- | --- |
| ![Comandos](../docs/images/commands.jpg) | ![Proxy](../docs/images/console-proxy.jpg) |

## Instalação binária

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | sh
```

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy-binary.sh | \
  HIDECK_DIR=/opt/hideck \
  HIDECK_VERSION=v2.1.22 \
  HIDECK_ARCH=linux_amd64 sh
```

| Arquivo | Plataforma |
| --- | --- |
| `hideck_v2.1.22_linux_amd64` | x86_64, glibc + UPX |
| `hideck_v2.1.22_linux_arm64` | ARM64 / Raspberry Pi OS 64-bit, glibc + UPX |
| `hideck_v2.1.22_linux_armv7` | ARM 32-bit, glibc + UPX |
| `hideck_v2.1.22_openwrt_amd64` | OpenWrt x86_64, musl estático, sem UPX |
| `hideck_v2.1.22_openwrt_arm64` | OpenWrt aarch64 |
| `hideck_v2.1.22_openwrt_armv7` | OpenWrt ARM 32-bit |

No OpenWrt use só `openwrt_*`. Veja [packaging/openwrt/README.md](../packaging/openwrt/README.md).

## Configuração

Copie [config/config.example.yaml](../config/config.example.yaml) para `config/config.yaml`.

| Chave | Padrão | Notas |
| --- | --- | --- |
| `server.port` | `7575` | HTTP |
| `server.https_enabled` | `false` | HTTPS embutido; deixe desligado atrás de reverse proxy |
| `server.https_port` | `7576` | Porta HTTPS embutida |
| `server.webrtc_udp_address` | `:7580` | UDP de áudio WebRTC |
| `server.webrtc_public_host` | vazio | Host ICE público atrás de NAT |
| `web.username` / `web.password` | `admin` | Troque imediatamente |
| `vowifi.enabled` | `false` | VoWiFi global |
| `system.openwrt_dynamic_interfaces` | `false` | Mapeamento netifd do OpenWrt |

Não faça commit de segredos. Os campos de PIN do SIM guardam só *nomes* de variáveis de ambiente (`HIDECK_SIM_PIN_READER1`). `PROXY_WEB_PASSWORD` sobrescreve o arquivo.

## Compilação

Go `1.26.4+`, Node.js / npm. `make build-*` precisa de UPX.

```bash
make build-amd64    # também: build-arm64, build-armv7, build-all
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

Proxy de desenvolvimento: `VITE_API_PROXY_TARGET=http://127.0.0.1:7575` em `web/.env.local`. API: `/api/docs`.

## Licença

Aprendizado, pesquisa e testes pessoais. Não é uma stack de telefonia de produção. Independente da Quectel, Qualcomm e das operadoras. Cumpra a lei local e os termos da sua operadora.

[PolyForm Noncommercial 1.0.0](../LICENSE). `third_party/vowifi-go` é AGPL-3.0. Leia [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) antes de distribuir binários ou imagens.

Thanks: [LINUX DO](https://linux.do/) · [iniwex5/vohive-release](https://github.com/iniwex5/vohive-release) · [boa-z/vowifi-go](https://github.com/boa-z/vowifi-go)
