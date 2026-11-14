<p align="center"><img src="docs/logo.svg" width="120" alt="compose-installer logo"></p>

# compose-installer

[![CI](https://github.com/LinuxSuRen/compose-installer/actions/workflows/ci.yml/badge.svg)](https://github.com/LinuxSuRen/compose-installer/actions/workflows/ci.yml)
[![Release](https://github.com/LinuxSuRen/compose-installer/actions/workflows/release.yml/badge.svg)](https://github.com/LinuxSuRen/compose-installer/actions/workflows/release.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**基于 [docker compose](https://docs.docker.com/compose/) 的通用产品打包与安装工具——离线/在线双模，Web 与手机 App 皆可操作。**

把 compose 产品交付到客户机器，通常意味着一堆手写脚本：逐个拉镜像、`docker save`、拷配置、在离线 Ubuntu 上装 docker、生成 `.env`，然后祈祷。compose-installer 把这套流程变成两个按钮：

- **打包**（在有网、装有 docker 的机器上）：从 compose 文件解析全量镜像清单，按目标架构拉取，连同项目文件子集与 docker/compose 安装物料，压成**单一 tar.gz**；
- **安装**（本机或 SSH 远程）：上传安装包 → 缺 docker 时安装（离线物料或 `get.docker.com`）→ 导入镜像 → 组装 `.env` → `docker compose up -d`，全程实时日志。

零配置即可用：指向任意 compose 项目直接打包；加一份 [`compose-installer.json`](docs/config.md) 即可解锁多产品/多环境、版本清单、必填环境变量等能力。

English documentation: [README.md](README.md)

## 特性亮点

保留的是实战细节，不只是管道：

- **离线/在线双模、单一格式**——安装端依据清单自动分支（[格式规范](docs/bundle-format.md)）；在线包仅 KB 级，镜像由目标机自行拉取；
- **跨架构安全**：多架构镜像**按平台 digest 拉取后回指 tag**，`docker save` 精确导出目标平台（规避 containerd 存储下 `content digest not found` 陷阱）；安装端比对 `uname -m`，不一致在上传前就硬失败；
- **按 profile 裁剪的包**：所选 compose profile 固化进编排并剥离 `profiles:` 标记——包自包含，目标机无需 `--profile`；
- **务实的 SSH 安装**：私钥/agent/密码认证链、三级 sudo 提权（root → `sudo -n` → `sudo -S` 经 stdin）、scp→SFTP 降级、GB 级包 md5 增量上传、sftp 子系统自动启用；
- **宿主状态调理**：cgroup v2 宿主要求 docker ≥ 20.10；`daemon.json "iptables": false` 的 NAT 修复；`compose up` 前幂等（只升不降）提升 inotify/sysctl 配额；磁盘空间检测；
- **fail-closed 的 `.env`**：模板 + 版本清单 tag + 计算默认值 + 用户覆盖，`requiredEnv` 键在前后端双重校验——绝不静默继承开发默认值；
- **Web 界面 + REST API**：同一套 API 驱动移动端（[Android](https://github.com/LinuxSuRen/compose-installer-android)、[HarmonyOS](https://github.com/LinuxSuRen/compose-installer-harmony)），见 [docs/api.md](docs/api.md)；
- **`doctor` 子命令**：独立诊断容器出网（NAT）、inotify 配额、内核限额、docker 版本与 cgroup 形态，附可直接复制的修复命令。

## 安装

三条路径，同一版本：

```bash
# 1. go install（Go >= 1.25）
go install github.com/LinuxSuRen/compose-installer@latest

# 2. GitHub Releases 预编译二进制
#    https://github.com/LinuxSuRen/compose-installer/releases

# 3. 容器镜像（多架构；交付形态默认隐藏「打包」页）
docker run --rm -p 8765:8765 ghcr.io/linuxsuren/compose-installer
```

## 快速开始

```bash
compose-installer
# compose-installer v0.1.0 started: http://127.0.0.1:8765（自动打开浏览器）
```

1. **打包页**——选择 compose 项目目录、产品/环境/版本、目标架构；可选提供 [docker-offline 安装包镜像](docker-offline/)（让安装包能自举安装 docker）；点击开始，产物落在 `<项目>/dist/`；
2. **安装页**——选择安装包（自动展示清单概要与必填环境变量项）、选择本机或 SSH 远程、填写必填项、开始，实时查看日志。

建议先试自带的 [`example/`](example/) 示例项目——约 20 行配置演示版本清单、`requiredEnv` 与 `envRewrites`。

命令行参数：`-addr`（默认 `127.0.0.1:8765`，被占用自动向后尝试）、`-open-browser=false`、`-hide-pack-tab`（交付形态）、`-v`。诊断 docker 宿主机：

```bash
compose-installer doctor   # 退出码 0 健康 / 1 有问题 / 2 无法诊断
```

## 配置你的项目

无配置时任意 compose 项目可直接使用（自动发现 compose 文件，单一 `prod` 环境）。添加 [`compose-installer.json`](docs/config.md)：

```json
{
  "defaultProduct": "platform",
  "products": {
    "platform": {
      "composeFiles": {"prod": "docker-compose.yml", "test": "docker-compose.test.yml"},
      "versions": {"prod": "versions/prod.json"},
      "requiredEnv": ["JWT_SECRET"],
      "envRewrites": {"UPLOADS_HOST_PATH": "{installDir}/data/uploads"}
    }
  }
}
```

版本清单把主版本号映射到各组件镜像 tag——部分发版只更新有变化的组件：

```json
{"latest": "1.0", "versions": {"1.0": {"backend": "0.0.23", "frontend": "0.0.23"}}}
```

## docker 离线物料

`docker-offline/` 用于构建承载 docker/compose 二进制与 systemd 单元的伴生镜像，打包时提取进离线包：

```bash
cd docker-offline
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/linuxsuren/compose-installer/docker-offline:27.5.1 --push .
```

## 文档

- [项目配置](docs/config.md)
- [安装包格式](docs/bundle-format.md)
- [REST API（移动端契约）](docs/api.md)
- [示例项目](example/)

## 参与贡献

欢迎提交 Issue 与 Pull Request。较大的改动建议先开 Issue 讨论；PR 保持单一主题。许可证见 [LICENSE](LICENSE)（Apache-2.0）。

## 项目由来

沉淀自在产线上长期运行机器人平台部署工具：多架构、离线优先的 compose 产品打包与安装的实战行为（digest 拉取、sudo 提权链、NAT 修复、配额调优）原样保留，产品专属部分全部转为配置。
