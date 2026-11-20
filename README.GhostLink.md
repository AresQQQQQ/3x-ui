> **GhostLink 定制 3x-ui**
>
> 基于官方 3x-ui，增加 GhostLink 节点、授权申请和客户端配套。

# GhostLink 定制 3x-ui

## 项目内容

本项目是一个预编译的 3x-ui 面板，保留原版的管理页面和常用协议，并增加
GhostLink 节点功能。日常使用只需要拉取 GHCR 镜像，不需要自行编译。

镜像名称：

```text
ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

镜像是纯净面板镜像，不预置授权后的节点程序、许可证或私钥。未使用 GhostLink
时，原版 VLESS、VMess、Trojan 等功能照常使用。申请获准后，面板会完成节点
程序的安装和启动。

## 与上游 3x-ui 的关系

本项目基于 [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui) 定制，不是
3x-ui 官方发行版。

- 面板基础功能和正式支持的协议来自上游；
- 本项目增加 GhostLink 节点和授权流程；
- 本项目会跟进上游新版本，测试通过后再发布新镜像；
- 官方 3x-ui 的安装脚本和镜像不包含 GhostLink 功能。

## 快速搭建

准备一台 Linux 服务器并安装 Docker Engine，然后执行：

```bash
mkdir -p /opt/3x-ui-ghostlink/db

docker run -d \
  --network host \
  -e GHOSTLINK_LICENSE_HUB_URL=https://auth.114514707.xyz \
  -v /opt/3x-ui-ghostlink/db:/etc/x-ui \
  -v /etc/machine-id:/host/etc/machine-id:ro \
  -v /sys/class:/host/sys/class:ro \
  -v /sys/devices:/host/sys/devices:ro \
  ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

随后打开：

```text
http://服务器IP:2053
```

如果还需要启用面板的客户端 IP 限制和自动封禁，在 `docker run` 中增加：

```bash
--cap-add NET_ADMIN --cap-add NET_RAW
```

需要容器异常退出后自动重启时，增加 `--restart unless-stopped`。需要长期保存
证书或 ACME 状态时，再按需增加对应目录挂载。

<details>
<summary>希望把启动参数保存到文件</summary>

仓库附带 [docker-compose.ghostlink.yml](docker-compose.ghostlink.yml)，可直接
下载并启动：

```bash
mkdir -p /opt/3x-ui-ghostlink
cd /opt/3x-ui-ghostlink
curl -fLO https://raw.githubusercontent.com/AresQQQQQ/3x-ui-ghostlink/main/docker-compose.ghostlink.yml
docker compose -f docker-compose.ghostlink.yml up -d
```

</details>

## 申请 GhostLink 授权

1. 登录面板，进入“入站列表”。
2. 新建 `ghostlink` 入站。
3. 按页面提示填写备注，然后点击“申请授权并安装”。
4. 等待审批。审批通过后，面板会自动安装节点所需的 GhostLink 程序。
5. 状态变为就绪后，保存入站并添加客户端。

授权服务地址用于提交申请和查询进度，不是面板登录地址，也不承载节点流量。
授权完成后，GhostLink 节点由服务器本机运行。

## 下载客户端

用户设备需要 GhostLink 定制版 Mihomo 核心：

<https://github.com/AresQQQQQ/ghostlink-client/releases>

该仓库提供的是客户端核心，不包含独立图形界面。配置时优先导入面板生成的
订阅或分享链接，不需要手工填写节点参数。

## 两个仓库的关系

| 仓库 | 使用位置 | 作用 |
| --- | --- | --- |
| [`3x-ui-ghostlink`](https://github.com/AresQQQQQ/3x-ui-ghostlink) | 服务器 | 管理节点、申请授权、生成订阅或分享链接 |
| [`ghostlink-client`](https://github.com/AresQQQQQ/ghostlink-client) | 用户设备 | 提供能够连接 GhostLink 节点的 Mihomo 核心 |

完整使用顺序是：

1. 在服务器启动 `3x-ui-ghostlink`；
2. 在面板中完成 GhostLink 授权；
3. 创建节点和用户，生成订阅或分享链接；
4. 在用户设备加载 `ghostlink-client` 提供的核心并导入链接。

## 常见问题

### `docker pull` 失败

确认服务器可以访问 GHCR，然后重试：

```bash
docker pull ghcr.io/aresqqqqq/3x-ui-ghostlink:latest
```

`docker run` 会自动拉取镜像，因此最简启动不需要先执行 `pull`。需要检查镜像
是否可访问时，可以单独运行上面的命令。

### 授权申请一直无法提交

检查服务器时间是否准确，并确认容器能够访问
`https://auth.114514707.xyz`。

### 重建容器会丢失数据吗

保留 `/opt/3x-ui-ghostlink/db:/etc/x-ui` 挂载即可保留面板数据库以及授权后的
节点文件。
