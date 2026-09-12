# 使用 uv 运行 WeKnora MCP 服务器

> 更推荐使用`uv`来运行基于python的MCP服务。
>
> 生产 API 基地址统一为 `https://knora.moutai.com.cn/api/v1`。配置中的
> `<TENANT_API_KEY>` 仅为占位符；真实密钥应由客户端环境变量/密钥管理注入。

## 1. 安装 uv

```bash
# macOS/Linux
curl -LsSf https://astral.sh/uv/install.sh | sh

# 或使用 Homebrew (macOS)
brew install uv

# Windows
powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
```

## 2. MCP 客户端配置

### Claude Desktop 配置

在 Claude Desktop 设置中添加:

```json
{
  "mcpServers": {
    "weknora": {
      "args": [
        "--directory",
        "/path/WeKnora/mcp-server",
        "run",
        "run_server.py"
      ],
      "command": "uv",
      "env": {
        "WEKNORA_API_KEY": "<TENANT_API_KEY>",
        "WEKNORA_BASE_URL": "https://knora.moutai.com.cn/api/v1"
      }
    }
  }
}
```

### Cursor 配置

在 Cursor 中，编辑 MCP 配置文件 (通常在 `~/.cursor/mcp-config.json`):

```json
{
  "mcpServers": {
    "weknora": {
      "command": "uv",
      "args": [
        "--directory",
        "/path/WeKnora/mcp-server",
        "run",
        "run_server.py"
      ],
      "env": {
        "WEKNORA_API_KEY": "<TENANT_API_KEY>",
        "WEKNORA_BASE_URL": "https://knora.moutai.com.cn/api/v1"
      }
    }
  }
}
```

### KiloCode 配置

对于 KiloCode 或其他支持 MCP 的编辑器，配置如下:

```json
{
  "mcpServers": {
    "weknora": {
      "command": "uv",
      "args": [
        "--directory",
        "/path/WeKnora/mcp-server",
        "run",
        "run_server.py"
      ],
      "env": {
        "WEKNORA_API_KEY": "<TENANT_API_KEY>",
        "WEKNORA_BASE_URL": "https://knora.moutai.com.cn/api/v1"
      }
    }
  }
}
```

### 其他 MCP 客户端

对于一般 MCP 客户端配置:

```json
{
  "mcpServers": {
    "weknora": {
      "command": "uv",
      "args": [
        "--directory",
        "/path/WeKnora/mcp-server",
        "run",
        "run_server.py"
      ],
      "env": {
        "WEKNORA_API_KEY": "<TENANT_API_KEY>",
        "WEKNORA_BASE_URL": "https://knora.moutai.com.cn/api/v1"
      }
    }
  }
}
```
