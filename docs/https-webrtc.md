# HTTPS 与 WebRTC 部署指南

HiDeck 的网页、API、SSE 和 WebRTC 信令使用 HTTP/HTTPS；通话音频使用独立的 WebRTC UDP 媒体通道。两者可以使用相同的端口号，因为 TCP 和 UDP 是不同协议。

| 公网入口 | 用途 | 处理方 |
| --- | --- | --- |
| `443/TCP` | HTTPS 页面、API、SSE、WebRTC 信令 | Nginx、Caddy 或 Lucky |
| `443/UDP` | WebRTC 音频媒体 | HiDeck 直连或 Nginx/Lucky 转发 |

使用自定义端口时，可以把两项同时改为 `8443/TCP` 和 `8443/UDP`。浏览器入口相应变为 `https://hideck.example.com:8443`。

## 域名与网络要求

域名需要创建 `A` 或 `AAAA` 记录并指向实际接收 TCP、UDP 流量的公网服务器。双栈部署同时创建 `A` 和 `AAAA`，并确保两个地址都能接收相同端口的 TCP、UDP。不要启用只能代理 HTTP 的 CDN 模式，否则域名解析出的地址无法直接接收 WebRTC UDP。

跨公网或存在 NAT 时，必须同时映射同一个数字端口的 TCP 和 UDP。`server.webrtc_public_host` 填写 HTTPS 使用的域名即可，HiDeck 启动时会检查域名解析，并在每次创建 WebRTC 媒体会话时重新解析 IPv4、IPv6 地址以发布公网 ICE 地址，不需要再填写公网 IP。DNS 更新生效后，新会话会使用新地址，无需重启；已有会话不会自动迁移到新地址。域名解析失败时，会明确报错，不会沿用旧 IP。

## 使用内置 HTTPS

不使用反向代理时，可以启用 HiDeck 内置 HTTPS：

```yaml
server:
  https_enabled: true
  https_port: 7576
  webrtc_udp_address: ":7580"
  webrtc_public_host: "hideck.example.com"
```

此时通过 `7576/TCP` 访问 HTTPS，并确保公网 IPv4、IPv6 可以访问 `7580/UDP`。

## Nginx 与 HiDeck 同机

这是同机部署的推荐方式。Nginx 只监听 `443/TCP`，HiDeck 直接监听 `443/UDP`。两者端口号相同但协议不同，不会冲突，也不需要使用 Nginx `stream` 转发 UDP。

三种网络模式对应的域名记录和 HiDeck UDP 地址如下：

| 模式 | DNS 记录 | `server.webrtc_udp_address` |
| --- | --- | --- |
| 仅 IPv4 | 只配置 `A` | `0.0.0.0:443` |
| 仅 IPv6 | 只配置 `AAAA` | `[::]:443` |
| IPv4+IPv6 双栈 | 同时配置 `A`、`AAAA` | `:443` |

公共 HiDeck 配置中的 `webrtc_udp_address` 按上表选择：

```yaml
server:
  port: 7575
  https_enabled: false
  webrtc_udp_address: "0.0.0.0:443"
  webrtc_public_host: "hideck.example.com"
```

仓库默认 Docker Compose 使用 host 网络和特权模式，可以直接监听 `443/UDP`。裸机以非 root 用户运行时，需要为低于 1024 的端口配置相应系统权限，或者改用 `8443`。

### Nginx 仅 IPv4

```nginx
server {
    listen 80;
    server_name hideck.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name hideck.example.com;

    ssl_certificate     /etc/letsencrypt/live/hideck.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hideck.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:7575;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 命令、电话和日志页面使用 SSE 长连接。
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

只创建 `A` 记录，并在 IPv4 防火墙或 NAT 中放行 `443/TCP`、`443/UDP`。

### Nginx 仅 IPv6

```nginx
server {
    listen [::]:80;
    server_name hideck.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen [::]:443 ssl;
    server_name hideck.example.com;

    ssl_certificate     /etc/letsencrypt/live/hideck.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hideck.example.com/privkey.pem;

    location / {
        proxy_pass http://[::1]:7575;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

只创建 `AAAA` 记录，并在 IPv6 防火墙中放行 `443/TCP`、`443/UDP`。IPv6 通常不需要 NAT，但防火墙必须允许入站流量。

### Nginx IPv4+IPv6 双栈

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name hideck.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name hideck.example.com;

    ssl_certificate     /etc/letsencrypt/live/hideck.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hideck.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:7575;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

同时创建 `A`、`AAAA` 记录，并分别在 IPv4、IPv6 防火墙中放行：

```text
443/TCP (IPv4/IPv6) -> Nginx 所在服务器
443/UDP (IPv4/IPv6) -> HiDeck 所在同一服务器
```

不要配置 `listen 443 quic` 或其他 HTTP/3 UDP 监听，否则 Nginx 会与 HiDeck 争用 `443/UDP`。

## Nginx 作为独立网关

当 Nginx 与 HiDeck 位于不同服务器时，Nginx 可以在公网网关上使用 `stream` 模块转发 UDP。HTTPS 配置仍使用上一节对应的 IPv4、IPv6 或双栈监听方式，只需把 `proxy_pass` 改成后端地址：

| 后端网络 | HTTPS 后端地址 | HiDeck UDP 地址 |
| --- | --- | --- |
| IPv4 | `http://192.168.1.20:7575` | `0.0.0.0:443` |
| IPv6 | `http://[fd00::20]:7575` | `[::]:443` |
| 双栈 | 选择任一可达后端地址 | `:443` |

HiDeck 的 `webrtc_public_host` 始终填写解析到 Nginx 公网网关的域名：

```yaml
server:
  port: 7575
  https_enabled: false
  webrtc_udp_address: "0.0.0.0:443"
  # 此域名解析到 Nginx 网关的公网地址。
  webrtc_public_host: "hideck.example.com"
```

UDP 配置位于与 `http` 同级的 `stream` 块，不能放进站点的 `server` 块。Nginx 必须包含或加载 `ngx_stream_module`。

### Nginx 网关仅 IPv4

```nginx
stream {
    server {
        listen 443 udp reuseport;
        proxy_pass 192.168.1.20:443;
        proxy_timeout 1h;
    }
}
```

公网只创建 `A` 记录并放行 IPv4 的 `443/TCP`、`443/UDP`。

### Nginx 网关仅 IPv6

```nginx
stream {
    server {
        listen [::]:443 udp reuseport;
        proxy_pass [fd00::20]:443;
        proxy_timeout 1h;
    }
}
```

公网只创建 `AAAA` 记录并放行 IPv6 的 `443/TCP`、`443/UDP`。

### Nginx 网关 IPv4+IPv6 双栈

```nginx
stream {
    server {
        listen 443 udp reuseport;
        proxy_pass 192.168.1.20:443;
        proxy_timeout 1h;
    }

    server {
        listen [::]:443 udp reuseport;
        proxy_pass [fd00::20]:443;
        proxy_timeout 1h;
    }
}
```

同时创建 `A`、`AAAA` 记录并放行双栈 `443/TCP`、`443/UDP`。如果后端只有 IPv4 或只有 IPv6，也可以让两个公网监听转发到同一个可达后端地址。

`reuseport` 用于让来自同一地址和端口的数据包进入同一个 UDP 会话。HiDeck 后端防火墙只需允许来自 Nginx 网关的 `443/UDP`。

当前默认 Compose 使用 host 网络。如果 Nginx 和 HiDeck 在同一主机上，不要让 Nginx `stream` 和 HiDeck 同时监听所有地址的 `443/UDP`；使用上一节的 UDP 直连方式即可。

## Lucky 作为网关

当 Lucky 运行在公网路由器或独立网关，HiDeck 位于内网时，需要分别创建 Web 服务规则和 UDP 端口转发规则。以下示例假设 HiDeck 地址为 `192.168.1.20`。

HiDeck 配置：

```yaml
server:
  port: 7575
  https_enabled: false
  webrtc_udp_address: ":443"
  # A 和 AAAA 都解析到 Lucky 网关。
  webrtc_public_host: "hideck.example.com"
```

### Lucky Web 服务规则

进入 `Web服务 -> Web服务规则列表 -> 添加Web服务规则`：

| 字段 | 填写 |
| --- | --- |
| 操作模式 | 简易模式 |
| 监听类型 | IPv4、IPv6 均勾选；只有单栈公网时只选对应类型 |
| 监听端口 | `443` |
| TLS | 开启 |
| 证书 | 选择 `hideck.example.com` 的证书 |
| Web 服务类型 | 反向代理 |
| 前端域名 | `hideck.example.com` |
| 后端地址 | `http://192.168.1.20:7575` |

使用 IPv6 后端时，后端地址填写 `http://[fd00::20]:7575`。Lucky Web 服务默认支持 WebSocket；HiDeck 的页面、API、SSE 和 WebRTC 信令都通过这条 HTTPS 规则。

关闭 Lucky 的 HTTP/3/QUIC，避免它占用 `443/UDP`。

### Lucky UDP 端口转发规则

进入 `端口转发 -> 转发规则列表 -> 添加转发规则`：

| 字段 | 填写 |
| --- | --- |
| 操作模式 | 简易模式 |
| 转发类型 | 同时勾选 `UDP4`、`UDP6`；只有单栈公网时只选对应类型 |
| 监听端口 | `443` |
| 目标地址 | `192.168.1.20`，IPv6 后端可填写 `fd00::20` |
| 目标端口 | `443` |
| 端口自动放行 | Lucky 运行在 OpenWrt 且由其管理防火墙时开启；Docker 环境关闭 |

不要在此规则中勾选 TCP，因为 `443/TCP` 已由 Lucky Web 服务处理。Lucky 可以在 IPv4、IPv6 监听侧和 IPv4、IPv6 后端之间进行端口转发，因此公网 IPv6 也可以转发到内网 IPv4 的 HiDeck。

最终链路：

```text
443/TCP (IPv4/IPv6) -> Lucky Web 服务 -> HiDeck 7575/TCP
443/UDP (IPv4/IPv6) -> Lucky 端口转发 -> HiDeck 443/UDP
```

Lucky 使用 Docker 运行时建议使用 host 网络，并自行配置宿主机 IPv4、IPv6 防火墙。如果 Lucky 前面还有上级路由器，需要把 `443/TCP` 和 `443/UDP` 的 IPv4 流量转发到 Lucky；IPv6 通常不做 NAT，但仍需在防火墙中放行。

如果 Lucky 与 HiDeck 在同一主机，Lucky Web 服务监听 `443/TCP`，HiDeck 直接监听 `443/UDP`，不要创建监听端口和目标端口同为 `443/UDP` 的回环转发规则。

## Caddy 自动部署

自动部署命令在三种网络模式下相同，实际对外协议栈由 DNS 记录和防火墙决定。Caddy 默认监听所有网络接口，HiDeck 默认使用 `:443` 监听 WebRTC UDP，因此无需修改容器配置即可使用 IPv4、IPv6 或双栈。

### Caddy 仅 IPv4

- 只创建域名 `A` 记录。
- 不创建 `AAAA` 记录。
- 在 IPv4 防火墙或 NAT 中放行 `443/TCP`、`443/UDP`。

### Caddy 仅 IPv6

- 只创建域名 `AAAA` 记录。
- 不创建 `A` 记录。
- 在 IPv6 防火墙中放行 `443/TCP`、`443/UDP`。
- IPv6 通常不需要 NAT，但必须允许公网入站。

### Caddy IPv4+IPv6 双栈

- 同时创建域名 `A`、`AAAA` 记录。
- 分别在 IPv4、IPv6 防火墙中放行 `443/TCP`、`443/UDP`。
- 两个 DNS 地址都必须真实可达，不能发布无法访问的 `A` 或 `AAAA`。

选择网络模式后执行：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_DIR=/opt/hideck sh
```

部署脚本会启用 `docker-compose.caddy.yml`，生成权限为 `0600` 的 `caddy.env` 与 `hideck-caddy.env`。Caddy 监听 `443/TCP`，HiDeck 监听 `443/UDP`。Caddy 配置关闭 HTTP/3，避免占用同一个 UDP 端口。

无法开放标准 ACME 端口时，可以使用自定义端口和 DNS-01。Cloudflare 示例：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com \
  HIDECK_HTTPS_PORT=8443 \
  HIDECK_DNS_PROVIDER=cloudflare \
  CLOUDFLARE_API_TOKEN=your_token \
  HIDECK_DIR=/opt/hideck sh
```

此时 Caddy 使用 `8443/TCP`，HiDeck 使用 `8443/UDP`，浏览器通过 `https://hideck.example.com:8443` 访问。DNS-01 不需要开放 `80/TCP` 或 `443/TCP` 用于证书验证，但实际业务端口的 TCP 和 UDP 都必须可达。

## Caddy 手工部署

Caddy 使用相同数字端口的 TCP，HiDeck 使用 UDP，因此三种模式都必须关闭 Caddy HTTP/3。

### Caddy 仅 IPv4

只配置 `A` 记录。Caddyfile：

```caddyfile
{
    servers {
        protocols h1 h2
    }
}

https://hideck.example.com:8443 {
    bind 0.0.0.0
    reverse_proxy 127.0.0.1:7575
}
```

HiDeck 配置：

```yaml
server:
  https_enabled: false
  webrtc_udp_address: "0.0.0.0:8443"
  webrtc_public_host: "hideck.example.com"
```

仅放行 IPv4 的 `8443/TCP`、`8443/UDP`。

### Caddy 仅 IPv6

只配置 `AAAA` 记录。Caddyfile：

```caddyfile
{
    servers {
        protocols h1 h2
    }
}

https://hideck.example.com:8443 {
    bind [::]
    reverse_proxy [::1]:7575
}
```

HiDeck 配置：

```yaml
server:
  https_enabled: false
  webrtc_udp_address: "[::]:8443"
  webrtc_public_host: "hideck.example.com"
```

仅放行 IPv6 的 `8443/TCP`、`8443/UDP`。

### Caddy IPv4+IPv6 双栈

同时配置 `A`、`AAAA` 记录。Caddyfile：

```caddyfile
{
    servers {
        protocols h1 h2
    }
}

https://hideck.example.com:8443 {
    bind 0.0.0.0 [::]
    reverse_proxy 127.0.0.1:7575
}
```

HiDeck 配置：

```yaml
server:
  https_enabled: false
  webrtc_udp_address: ":8443"
  webrtc_public_host: "hideck.example.com"
```

分别放行 IPv4、IPv6 的 `8443/TCP`、`8443/UDP`。

使用仓库 Compose 手工启动时，`caddy.env` 在三种模式下相同：

```dotenv
HIDECK_DOMAIN=hideck.example.com
HIDECK_HTTPS_PORT=8443
```

`hideck-caddy.env` 根据模式填写对应的 UDP 监听地址：

| 模式 | `PROXY_SERVER_WEBRTC_UDP_ADDRESS` |
| --- | --- |
| 仅 IPv4 | `0.0.0.0:8443` |
| 仅 IPv6 | `[::]:8443` |
| IPv4+IPv6 双栈 | `:8443` |

双栈示例：

```dotenv
PROXY_SERVER_WEBRTC_PUBLIC_HOST=hideck.example.com
PROXY_SERVER_WEBRTC_UDP_ADDRESS=:8443
```

启动：

```bash
docker compose -f docker-compose.yml -f docker-compose.caddy.yml up -d
```

Caddy 的证书状态保存在 Compose 的 `caddy_data` 卷中。

## DNS-01 服务商

设置 `HIDECK_DNS_PROVIDER` 后，部署脚本会拉取预构建的 `yibaiba/hideck-caddy-dns:2.11.4`。该镜像同时包含四个 DNS 模块，实际使用的模块由 `HIDECK_DNS_PROVIDER` 决定。凭证仅写入权限为 `0600` 的 `caddy.env` 并注入 Caddy，不会进入镜像或 HiDeck 容器。

| `HIDECK_DNS_PROVIDER` | 必需环境变量 |
| --- | --- |
| `cloudflare` | `CLOUDFLARE_API_TOKEN` |
| `alidns` | `ALIYUN_ACCESS_KEY_ID`、`ALIYUN_ACCESS_KEY_SECRET` |
| `tencentcloud` | `TENCENTCLOUD_SECRET_ID`、`TENCENTCLOUD_SECRET_KEY` |
| `route53` | `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`，可选 `AWS_REGION`；EC2 实例角色可不填密钥 |

阿里云示例：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com HIDECK_HTTPS_PORT=8443 \
  HIDECK_DNS_PROVIDER=alidns \
  ALIYUN_ACCESS_KEY_ID=your_id ALIYUN_ACCESS_KEY_SECRET=your_secret \
  HIDECK_DIR=/opt/hideck sh
```

腾讯云 DNSPod 示例：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com HIDECK_HTTPS_PORT=8443 \
  HIDECK_DNS_PROVIDER=tencentcloud \
  TENCENTCLOUD_SECRET_ID=your_id TENCENTCLOUD_SECRET_KEY=your_key \
  HIDECK_DIR=/opt/hideck sh
```

AWS Route53 示例：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | \
  HIDECK_DOMAIN=hideck.example.com HIDECK_HTTPS_PORT=8443 \
  HIDECK_DNS_PROVIDER=route53 \
  AWS_ACCESS_KEY_ID=your_id AWS_SECRET_ACCESS_KEY=your_secret AWS_REGION=us-east-1 \
  HIDECK_DIR=/opt/hideck sh
```

Cloudflare Token 需要目标 Zone 的 `Zone:Read` 和 `DNS:Edit`。其他服务商也应限制为目标域名的 DNS 查询和 TXT 记录修改权限，不要使用账号主密钥。

## 构建预置 DNS 模块的 Caddy

镜像构建文件集中在 `caddy-dns-image/`：

```bash
cd caddy-dns-image
CADDY_VERSION=2.11.4 docker compose build --push
```

该命令构建并推送 `linux/amd64`、`linux/arm64`，同时生成 `2.11.4` 和 `latest` 标签。通过 `CADDY_LATEST_TAG` 可以修改第二个标签。

## 参考资料

- [Nginx HTTP proxy module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)
- [Nginx stream proxy module](https://nginx.org/en/docs/stream/ngx_stream_proxy_module.html)
- [Nginx stream listen](https://nginx.org/en/docs/stream/ngx_stream_core_module.html#listen)
- [Lucky Web 服务](https://lucky666.cn/docs/modules/web)
- [Lucky 端口转发](https://lucky666.cn/docs/modules/portforward)
- [Caddy Automatic HTTPS](https://caddyserver.com/docs/automatic-https)
- [Caddy bind](https://caddyserver.com/docs/caddyfile/directives/bind)
- [Caddy protocols](https://caddyserver.com/docs/caddyfile/options#protocols)
