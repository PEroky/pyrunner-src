# 🐍 PyRunner

轻量级 Python 脚本管理面板。Go 单二进制，内嵌前端，零依赖。

## 功能

- 📝 脚本管理（创建/编辑/删除）
- ▶️ 一键运行，SSE 实时输出流
- ⏹ 中途停止脚本
- 📋 运行历史记录
- 📦 pip 包管理（安装/卸载/搜索）
- 🔒 可选账号密码保护

## 快速开始

### Docker (推荐)

```bash
docker run -d \
  --name pyrunner \
  -p 8000:8000 \
  -v pyrunner-data:/data \
  -e PYRUNNER_USER=admin \
  -e PYRUNNER_PASS=your_password \
  ghcr.io/YOUR_USERNAME/pyrunner:latest
```

### Docker Compose

```yaml
services:
  pyrunner:
    image: ghcr.io/YOUR_USERNAME/pyrunner:latest
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
# 下载源码构建
git clone https://github.com/YOUR_USERNAME/pyrunner.git
cd pyrunner
go build -o pyrunner .

# 运行
export PYRUNNER_USER=admin
export PYRUNNER_PASS=your_password
./pyrunner
```

## 配置

通过环境变量配置：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PYRUNNER_PORT` | `8000` | 服务端口 |
| `PYRUNNER_USER` | 空 | 管理员用户名，空=不开启认证 |
| `PYRUNNER_PASS` | 空 | 管理员密码，空=不开启认证 |
| `PYRUNNER_DATA` | `.` | 数据目录（存放 DB 和脚本） |

> ℹ️ 当 `PYRUNNER_USER` 和 `PYRUNNER_PASS` 均为空时，不开启认证，任人可访问。

## 资源占用

| 资源 | 占用 |
|------|------|
| 二进制 | ~15MB |
| 运行内存 | ~13MB |
| Docker 镜像 | ~150MB |
| CPU | ≈ 0%（空闲） |

## 构建镜像

推送 tag 自动触发 GitHub Actions 构建：

```bash
git tag v1.0.0
git push origin v1.0.0
```

镜像发布到 `ghcr.io`，支持 `linux/amd64` 和 `linux/arm64`。

## License

MIT
