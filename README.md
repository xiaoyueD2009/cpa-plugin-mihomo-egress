# CPA Mihomo Router

这是一个独立的 CPA v8.0.4 动态库插件，参考 [CPA Usage Keeper Plugin](https://github.com/Willxup/cpa-plugin-usage-keeper) 的注册和管理页接入方式。CPA 插件栏显示为 **Mihomo Router**；页面也可直接通过 `/v0/resource/plugins/mihomo-egress/open` 打开。它只查看 Mihomo 中允许的代理组、当前节点、候选节点、类型、地址与健康信息，并允许手动选择组内节点；不编辑配置文件，也不提供节点新增、删除或修改功能。

插件拥有独立的 ID（`mihomo-egress`）、动态库文件、CPA 配置项和管理页面。它不修改 Keeper、CPA 主程序或其他插件。CPA 自身可按此 ID 单独启用、禁用、配置和卸载。

## CPA 插件商店安装

插件商店可以按 CPA 运行平台自动选择并校验对应的插件包，用户不必自行安装 Go 或手动复制动态库。官方商店要求插件仓库提供 `vX.Y.Z` GitHub Release、平台 ZIP 和 `checksums.txt`，并由插件作者向 [CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) 提交目录条目。本仓库的 [store/registry-entry.json](store/registry-entry.json) 是供官方目录 PR 使用的条目。

官方目录收录后，在 CPA 插件商店搜索 **CPA Mihomo Router**（插件 ID：`mihomo-egress`）并安装即可。安装二进制后仍需在 CPA 插件管理页单独填写 Mihomo Controller 地址、可选的 Controller 密钥和允许的代理组；插件不会从 Mihomo 配置中复制密钥，也不会自动修改 CPA 配置。当前 CPA 版本如果没有商店界面，仍可按下方手动安装方式部署。

对 fallback 组，手动切换会通过 Mihomo Controller 的 `PUT /proxies/{组名}` 设置运行态固定节点。Mihomo 官方 API 文档列出 `now`、`all` 与 `fixed` 字段，以及该 PUT 选择接口：[Mihomo APIs](https://wiki.metacubex.one/api/)。插件不会调用 `DELETE /proxies/{组名}` 清除固定选择；如需恢复自动回退，请通过 Mihomo 自己的管理方式操作。

## 独立安装、配置与卸载

插件按 CPA 的插件 ID 独立管理，不需要改 CPA 主程序，也不需要安装或修改 Keeper。CPA 的全局插件加载器需处于启用状态；实际配置、启停和删除仅影响 `mihomo-egress`。

在有 Go 1.26、CGo 和目标系统 C 编译器的机器上构建单独的插件文件：

```bash
make build GOOS=linux GOARCH=amd64
```

将生成的 `dist/linux/amd64/mihomo-egress.so` 单独放入 CPA 的插件目录，例如：

```text
/opt/cliproxy-api/plugins/linux/amd64/mihomo-egress.so
```

然后在 CPA 的插件管理页中启用 `mihomo-egress` 并填写该插件自己的配置；也可在配置文件中仅添加以下独立配置段：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    mihomo-egress:
      enabled: true
      priority: 1
      controller_url: http://mihomo:9090
      controller_secret: "<Mihomo 的 secret；未设置时省略>"
      group_names: "AI-EXIT,JP-EXIT"
      access_key: "<至少 32 个字符的插件独立访问密钥>"
```

若全局 `plugins.enabled` 已打开，不必重复修改它。`controller_url`、`controller_secret`、`group_names` 和 `access_key` 都是这个插件自己的设置。生成高熵访问密钥可使用 `openssl rand -hex 32`，不要复用 CPA 管理密钥。CPA 与 Mihomo 共用 Docker 网络时，`http://mihomo:9090` 通常是容器内地址；若 Mihomo 服务名不同，只填 CPA 容器可达的 Controller 地址。不要把 Controller 端口开放到公网。`group_names` 支持英文逗号分隔多个 Mihomo 组名；当前只配置 `AI-EXIT` 时页面只有一个组，配置多个组后可用页面选择器切换。

页面直接打开后输入插件自己的 `access_key`；它只保存在当前页面内存中，不写入浏览器存储。插件在 CPA 服务端独立校验该密钥，不调用 CPA 管理 API，也不会把 `access_key` 或 Mihomo 的 `controller_secret` 返回给浏览器。

卸载时，在 CPA 插件管理页禁用并移除 `mihomo-egress`，然后从插件目录删除 `mihomo-egress.so`；若使用文件配置，再只删除 `plugins.configs.mihomo-egress` 这一段。其他插件与 CPA 配置不受影响。禁用或卸载不会删除 Mihomo 节点定义；Mihomo 已固定的 fallback 节点由 Mihomo 保持当前运行态。

## 安全边界

- 插件页面、状态和切换均使用 CPA 提供的插件资源路由；所有接口由插件自身的 `access_key` 保护，CPA 管理密钥不参与。由于 CPA 插件资源路由只接受 GET，切换操作使用带密钥请求头、同源校验和禁用缓存的插件资源请求。
- `controller_secret` 只由 CPA 服务端添加到 Mihomo Controller 请求头。插件只向浏览器返回筛选后的节点字段，不返回原始代理对象或代理凭据。
- Controller 请求禁用环境代理并拒绝重定向。页面只向同源 CPA API 发请求，切换处理器拒绝标记为跨站的请求。
- `group_names` 是服务端允许列表。切换前会从 Mihomo 实时验证所选节点属于该组。
- 插件只调用 Mihomo `/proxies` GET 和 `/proxies/{group}` PUT，不调用 `/configs`、`/restart` 或删除接口。

## 构建与验证

```bash
make test
make vet
make build
make package
```

`make package` 生成仅包含该插件动态库的独立 ZIP 包和 SHA-256 校验文件。Linux amd64 可在目标服务器构建：

```bash
make build GOOS=linux GOARCH=amd64 PLUGIN_OUTPUT=dist/linux/amd64/mihomo-egress.so
```

在 macOS 上交叉构建 Linux 动态库时，还需安装 Linux amd64 交叉 C 编译器，并设置 `CC`，例如：

```bash
CC=x86_64-linux-gnu-gcc make build-linux-amd64
```

推送 `vX.Y.Z` 标签会触发 [GitHub Actions 发布流程](.github/workflows/release.yml)，为 macOS amd64/arm64、Linux amd64/arm64 和 Windows amd64 构建商店兼容 ZIP，并汇总生成 `checksums.txt`。ZIP 根目录只含对应平台的 `mihomo-egress.dylib`、`.so` 或 `.dll`。发布资产命名遵循官方商店约定：

```text
mihomo-egress_<version>_<goos>_<goarch>.zip
checksums.txt
```

商店目录收录由 CLIProxyAPI 官方仓库审核；后续插件版本只需发布新标签和资产，无需重复修改目录。

## 实现说明

CPA 插件 ID 按二进制文件名去掉扩展名确定，因此构建和部署文件名固定为 `mihomo-egress`。切换记录最多保留 20 条，保存在插件进程内存中，CPA 重启后清空。本仓库只提供该插件源码、构建和独立安装说明，不会自动部署到服务器。
