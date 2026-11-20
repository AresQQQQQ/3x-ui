> **GhostLink 定制 3x-ui**
>
> 本文档面向最终用户，提供预编译纯净镜像的一键部署和授权激活流程。

# GhostLink 定制 3x-ui

本项目基于 [3x-ui](https://github.com/MHSanaei/3x-ui)，增加原生
`ghostlink` 入站、授权激活和进程托管能力。镜像只包含面板与托管逻辑，不包含
`ghostlinkd` 核心二进制、源码、许可证文件或任何共享密钥。授权通过后，面板才会
从 License-Hub 申请并下载当前节点专用的核心。

未启用 GhostLink 入站时，面板不会启动 `ghostlinkd`，也不会访问 License-Hub。

> **镜像可见性**
>
> 首次发布的 GHCR 包默认可能是 Private。若 `docker pull` 返回未授权，请在
> GitHub Package settings 中将
> `AresQQQQQ/3x-ui-ghostlink` 的可见性调整为 Public。

## 一键部署

准备一台 Linux 服务器，安装 Docker Engine 与 Docker Compose v2。

```bash
mkdir -p /opt/3x-ui-ghostlink
cd /opt/3x-ui-ghostlink
curl -fLO https://raw.githubusercontent.com/AresQQQQQ/3x-ui-ghostlink/main/docker-compose.ghostlink.yml
docker compose -f docker-compose.ghostlink.yml pull
docker compose -f docker-compose.ghostlink.yml up -d
```

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
