> **GhostLink 定制 3x-ui**
>
> 本文档面向最终用户，提供预编译纯净镜像的一键部署和授权激活流程。

# GhostLink 定制 3x-ui

本项目基于 [3x-ui](https://github.com/MHSanaei/3x-ui)，增加原生
`ghostlink` 入站、授权激活和进程托管能力。镜像只包含面板与托管逻辑，不包含
`ghostlinkd` 核心二进制、源码、许可证文件或任何共享密钥。授权通过后，面板才会
从 License-Hub 申请并下载当前节点专用的核心。

未启用 GhostLink 入站时，面板不会启动 `ghostlinkd`，也不会访问 License-Hub。

预编译镜像位于公开 GHCR：

```text
ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

该地址可匿名拉取，不需要源码构建，也不需要 GHCR 登录：

```bash
docker pull ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

`https://auth.114514707.xyz` 是面板申请授权时访问的 **License-Hub**
入口，由该服务的管理方通过 Cloudflare Tunnel 对外提供。它不是面板登录地址，
也不表示面板容器内含 `cloudflared`。面板自身仍通过服务器 IP 和面板端口访问。

## 部署方式

Docker Compose 不是运行镜像的必要条件。可以直接使用 `docker run`，也可以使用
模板提供的 Compose 文件。两种方式拉取的是同一个 GHCR 预编译镜像，选择其中一种
即可，不要同时创建同名容器。

### 方式一：直接运行 `docker run`

准备一台 Linux 服务器并安装 Docker Engine。

```bash
mkdir -p /opt/3x-ui-ghostlink/{db,cert,acme}
cd /opt/3x-ui-ghostlink
docker pull ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
docker run -d \
  --name xui-ghostlink \
  --network host \
  --cap-add NET_ADMIN \
  --cap-add NET_RAW \
  --restart unless-stopped \
  --tty \
  -e XRAY_VMESS_AEAD_FORCED=false \
  -e XUI_ENABLE_FAIL2BAN=true \
  -e GHOSTLINK_LICENSE_HUB_URL=https://auth.114514707.xyz \
  -e GHOSTLINK_GOMEMLIMIT=96MiB \
  -e XUI_GOGC=75 \
  -e XUI_MEMORY_LIMIT=256 \
  -v "$PWD/db:/etc/x-ui/" \
  -v /etc/machine-id:/host/etc/machine-id:ro \
  -v /sys/class:/host/sys/class:ro \
  -v /sys/devices:/host/sys/devices:ro \
  -v "$PWD/cert:/root/cert/" \
  -v "$PWD/acme:/root/.acme.sh/" \
  ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

该命令与下方的 Compose 模板等价。`--network host` 让面板和 GhostLink 直接使用
宿主机网络端口。必须保留 `./db` 挂载，否则升级或重建容器会丢失面板数据库、
已下载的 `ghostlinkd` 和 `license.lic`。

具体落盘位置为：

- `db/x-ui.db`：面板数据库
- `db/ghostlink/ghostlinkd`：授权后下载的核心
- `db/ghostlink/license.lic`：节点许可证

### 方式二：Docker Compose（可选）

Compose 把上面的参数保存在文件中，适合长期维护。准备 Docker Engine 与 Docker
Compose v2 后执行：

```bash
mkdir -p /opt/3x-ui-ghostlink
cd /opt/3x-ui-ghostlink
curl -fLO https://raw.githubusercontent.com/AresQQQQQ/3x-ui-ghostlink/main/docker-compose.ghostlink.yml
docker compose -f docker-compose.ghostlink.yml pull
docker compose -f docker-compose.ghostlink.yml up -d
```

Compose 文件中的 `image:` 已固定为上面的 GHCR 地址，因此 `pull` 拉取的是
预编译成品，不会在服务器上构建源码。

选择 Compose 的原因是启动、升级、查看状态和日志更直观：

```bash
docker compose -f docker-compose.ghostlink.yml ps
docker compose -f docker-compose.ghostlink.yml logs --tail=200 xui
docker compose -f docker-compose.ghostlink.yml pull
docker compose -f docker-compose.ghostlink.yml up -d
```

它没有额外的运行依赖或镜像功能。若直接使用 `docker run`，故障排查命令可替换为
`docker ps`、`docker logs --tail=200 xui-ghostlink` 和 `docker pull`。

打开 `http://服务器地址:2053`，完成面板首次登录和安全设置。模板使用
`network_mode: host`，以保留 GhostLink 的 TCP/UDP 监听能力。请只在专用 VPS
上使用，并先确认宿主机对应端口可用。

模板会持久化以下目录：

- `./db` 保存面板数据库、已下载的 `ghostlinkd` 和 `license.lic`
- `./cert` 保存证书
- `./acme` 保存 ACME 续期状态
- `/etc/machine-id`、`/sys/class`、`/sys/devices` 以只读方式提供硬件指纹

内存参数已按小规格 VPS 预填。核心运行上限与面板上限的总占用仍需根据机器规格
调整，修改环境变量后执行 `docker compose -f docker-compose.ghostlink.yml up -d`
即可应用。

## 申请并激活 GhostLink

1. 登录面板，进入“入站列表”，新建 `ghostlink` 入站。
2. 未激活时页面会显示引导。核对脱敏后的 machine-id、DMI UUID 和 NIC 哈希。
3. 填写可识别的备注，例如主机商、地区或工单编号，然后点击申请授权。
4. 面板会向 `https://auth.114514707.xyz` 提交申请并显示审批进度。
5. 审批通过后，面板自动完成临时 X25519 协商、分块解密、SHA-256 校验和落盘。
6. 状态变为就绪后，创建入站并配置客户端。

Cloudflare Tunnel 在这里只承担 License-Hub 的公网 HTTPS 接入，不代理面板后台，
也不是 GhostLink 入站流量通道。已激活节点的 `ghostlinkd` 离线自治运行，不会
因为 Cloudflare 或 License-Hub 暂时不可达而停止转发。

授权未完成时不能创建有效 GhostLink 入站。不要重复提交大量申请，待审批数量存在
队列上限。

## 客户端

使用 GhostLink 定制版 Mihomo 客户端核心：

<https://github.com/AresQQQQQ/ghostlink-client/releases>

项目源码保持私有，公开发布仓库只分发签核通过的预编译核心、校验和及使用说明。

## 故障排查

```bash
docker compose -f docker-compose.ghostlink.yml ps
docker compose -f docker-compose.ghostlink.yml logs --tail=200 xui
docker compose -f docker-compose.ghostlink.yml pull
docker compose -f docker-compose.ghostlink.yml up -d
```

若始终无法申请授权，先检查容器时间、到
`https://auth.114514707.xyz` 的网络、`GHOSTLINK_LICENSE_HUB_URL` 和中国大陆网络
环境对该域名的可达性。
