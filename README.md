# 🐍 PyRunner

轻量级 Python 脚本管理面板。Go 单二进制，内嵌前端，零依赖。

## 功能

- 📝 脚本管理（创建/编辑/删除）
- ▶️ 一键运行，SSE 实时输出流
- ⏹ 中途停止脚本
- 💾 代码自动保存（1.5s 防抖）
- 🖥️ Web 终端（xterm.js + WebSocket PTY）
- 📦 pip 包管理（安装/卸载/搜索），包列表持久化
- 🔒 可选账号密码保护
- 🌗 亮色/暗色主题切换
- 🛡️ 内置 Xray（Docker 镜像）

## 快速开始

### Docker (推荐)

```bash
docker run -d \
  --name pyrunner \
  -p 8000:8000 \
  -v pyrunner-data:/data \
  -e PYRUNNER_USER=admin \
  -e PYRUNNER_PASS=your_password \
  ghcr.io/peroky/pyrunner-src:latest
```

### Docker Compose

```yaml
services:
  pyrunner:
    image: ghcr.io/peroky/pyrunner-src:latest
    container_name: pyrunner
    restart: unless-stopped
    ports:
      - "8000:8000"
    volumes:
      - pyrunner-data:/data
    environment:
      - PYRUNNER_USER=admin
      - PYRUNNER_PASS=your_password

volumes:
  pyrunner-data:
```

### 二进制部署

```bash
git clone https://github.com/PEroky/pyrunner-src.git
cd pyrunner-src
go build -o pyrunner .

export PYRUNNER_USER=admin
export PYRUNNER_PASS=your_password
./pyrunner
```

## 配置

通过环境变量配置：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PYRUNNER_PORT` | `8000` | 服务端口（也可用 `PYRUNNER_LISTEN_PORT`） |
| `PYRUNNER_USER` | 空 | 管理员用户名，空=不开启认证 |
| `PYRUNNER_PASS` | 空 | 管理员密码，空=不开启认证 |
| `PYRUNNER_DATA` | `.` | 数据目录（存放 DB、脚本和 requirements.txt） |

> ℹ️ 当 `PYRUNNER_USER` 和 `PYRUNNER_PASS` 均为空时，不开启认证。

## pip 包持久化

安装/卸载包后，PyRunner 自动执行 `pip freeze` 并保存到 `$PYRUNNER_DATA/requirements.txt`。  
容器重启时自动从该文件恢复已安装的包，无需手动重新安装。

## Web 终端

内置 Web 终端基于 xterm.js + WebSocket PTY，可直接在浏览器中执行 shell 命令。  
适用于排查卡死进程、手动操作容器等场景。

## 资源占用

| 资源 | 占用 |
|------|------|
| 二进制 | ~15MB |
| 运行内存 | ~13MB |
| Docker 镜像 | ~180MB（含 Xray） |
| CPU | ≈ 0%（空闲） |

## 构建镜像

推送到 main 分支自动触发 GitHub Actions 构建。
镜像发布到 `ghcr.io`，支持 `linux/amd64` 和 `linux/arm64`。

## License

MIT
