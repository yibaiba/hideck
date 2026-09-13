# HiDeck Docker Hub 镜像

镜像地址：`yibaiba/hideck`

支持架构：

- `linux/amd64`
- `linux/arm64`

## 快速启动（推荐）

直接通过 curl 运行部署脚本，默认安装到当前目录下的 `hideck/`：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | sh
```

自定义安装目录：

```bash
curl -fsSL https://raw.githubusercontent.com/yibaiba/hideck/main/deploy.sh | HIDECK_DIR=/opt/hideck sh
```

脚本会下载 `docker-compose.yml` 和配置模板，创建持久化目录并拉取 `latest`；不会覆盖已有的部署文件和 `config/config.yaml`。

## 手工部署

```bash
mkdir -p hideck/{config,data,logs}
cd hideck
```

创建 `config/config.yaml`：

```yaml
server:
  port: 7575
  debug: false
  https_enabled: false

web:
  username: admin
  password: admin

devices: []

proxy:
  instances: []

vowifi:
  enabled: false
```

创建 `docker-compose.yml`：

```yaml
services:
  hideck:
    image: yibaiba/hideck:latest
    container_name: hideck
    restart: unless-stopped
    init: true
    stop_grace_period: 30s
    network_mode: host
    privileged: true
    volumes:
      - ./config:/app/config
      - ./data:/app/data
      - ./logs:/app/logs
      - /dev:/dev
    environment:
      TZ: Asia/Shanghai
      CONFIG_PATH: /app/config/config.yaml
    logging:
      driver: json-file
      options:
        max-size: 10m
        max-file: "3"
```

启动：

```bash
docker compose up -d
```

Web 入口：`http://YOUR_IP:7575`

默认账号：`admin` / `admin`

首次登录后请立即修改密码。

## 维护者发布

发版镜像不再在 Docker 里编译或 `apk`。先打好 `dist/hideck_vX.Y.Z_linux_amd64` 和 `linux_arm64`，再拷进运行时底包。

原来的源码构建还在：根目录 `Dockerfile` + `docker-compose.source.yml`。更新 Alpine/录音库等依赖时走这条，或先重建 `hideck-runtime`，后面的发版镜像才会用到新底包。

运行时底包（`ca-certificates`、AMR/MP3、`gcompat`、`qmi-proxy`）只在依赖变化时重建：

```bash
docker compose -f docker-compose.runtime.yml build --builder hideck-release --push
```

arm64 拉 Alpine 包若 TLS 失败，改用：

```bash
docker buildx build --builder hideck-release --allow network.host \
  --platform linux/amd64,linux/arm64 -f Dockerfile.runtime \
  -t yibaiba/hideck-runtime:3.24 -t yibaiba/hideck-runtime:latest --push .
```

每次发版：

```bash
# 先 make / 本地编出 UPX 后的 dist/hideck_v2.1.22_linux_amd64 和 linux_arm64
export HIDECK_VERSION=2.1.22
export HIDECK_MINOR_VERSION=2.1
export HIDECK_REVISION="$(git rev-parse HEAD)"
export HIDECK_BUILDTIME="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

docker compose -f docker-compose.build.yml build --builder hideck-release --push
docker buildx imagetools inspect "yibaiba/hideck:${HIDECK_VERSION}"
```

服务器部署仍只用 `docker-compose.yml` 拉 `yibaiba/hideck:latest`，不会在服务器编译。

从源码完整构建（更新依赖或不用预编译二进制）：

```bash
export HIDECK_VERSION=2.1.22
export HIDECK_MINOR_VERSION=2.1
export HIDECK_REVISION="$(git rev-parse HEAD)"
export HIDECK_BUILDTIME="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
docker compose -f docker-compose.source.yml build --builder hideck-release --push
```

## 更新镜像

```bash
docker compose pull
docker compose up -d
```

应用内二进制热更新在这个源码整合构建中已禁用。Docker 部署请通过拉取新镜像升级。

## 配置说明

| 路径 | 说明 |
| --- | --- |
| `/app/config` | 配置文件目录 |
| `/app/data` | SQLite 数据与运行数据 |
| `/app/logs` | 日志目录 |

容器默认时区为 `Asia/Shanghai`。Compose 文件也显式设置了同一时区，方便在不同运行方式下保持一致。

## 许可证提示

本仓库是源码整合树，不是单一 MIT 许可项目。根项目来自 PolyForm Noncommercial 1.0.0，`third_party/vowifi-go` 为 AGPL-3.0，其它第三方源码按各自许可证授权。发布公开二进制或 Docker 镜像前，请先确认组合分发的许可证义务。
