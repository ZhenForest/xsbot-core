# xsbot-core

单账号、轻量的 Go QQ Bot Core。通过一条 OneBot 11 通用正向 WebSocket 连接独立运行的协议端；业务由可信 Go 插件实现。

Core 包含严格消息解析、Rule 路由、按会话保序的有界调度、按需一层引用补取、媒体资源管理、插件生命周期、HTTPS 管理页和日志。公开目录仅包含生产源码、SDK、构建依赖及许可证；没有业务插件、测试代码、测试脚本、离线表情名称表或私人部署资料。

## 构建

正式运行要求 **Linux、Go 1.27.3、CGO_ENABLED=1**。依赖已包含在 vendor 中：

```sh
CGO_ENABLED=1 go build -mod=vendor -trimpath -o build/bot-core ./cmd/bot-core
```

模块路径：`github.com/ZhenForest/xsbot-core`；公开接口见 [sdk/types.go](sdk/types.go)、[sdk/configuration.go](sdk/configuration.go) 和 [sdk/documents.go](sdk/documents.go)。

## 运行

```sh
./build/bot-core run --dir ./run
```

第一次运行会生成 `run/config.yaml` 占位模板后退出。填写你自己的 OneBot WS 根地址、Token、浏览器访问 IP 等必要字段，再执行：

```sh
./build/bot-core check-config --dir ./run
./build/bot-core run --dir ./run
```

协议端需独立安装、登录和启动。同一条通用 WS 同时处理 API 与事件，不需要另建 HTTP API 连接。不要把 Token 放入 URL。

首次正常启动会在终端输出临时管理密码；通过配置 IP 和端口访问 HTTPS 页面，核对自签名证书并修改临时密码。Web 按地址族监听所有接口，`public_ip` 用于证书而非绑定白名单；由部署者控制管理入口的网络访问。

新发现插件默认登记为空 Rule，不开放任何会话。Rule 和业务配置由使用者自行管理。运行配置、认证、证书、日志和插件数据只放在运行目录，不提交到源码仓库。

## 插件与边界

插件导出 Descriptor 和 New，使用 SDK 的 Init／Deliver／Close 生命周期与 Host 接口。Deliver 应快速接收至自己的有界队列，长任务响应 context 取消。

插件与 Core 同进程运行，是可信代码，不提供安全沙箱或热卸载。Core 与 `.so` 必须使用匹配的 Go 工具链、SDK／共享源码、依赖、CGO 和构建标志；模块路径或共享包改变后应匹配重建。

当前处理好友私聊和普通群聊；发送结果只表示协议端报告成功。超时或断线不保证上游没有执行，Core 不自动补发。SDK 表情方法只使用协议上报名称，名称缺失时保留 ID。

## 可选 SDK 能力

- 实现 `sdk.QuotedMessageConsumer` 并返回 `NeedsQuotedMessage() == true`，可在 Rule 匹配和权限通过后取得当前消息的直接引用正文。最多补取一层，整条消息共享 5 秒期限、全局 4 路查询；失败通过 `QuoteStatus` 表达，原消息仍可投递。引用正文不参与 Rule 匹配，不递归展开或下载媒体。
- 同会话在 Core 投递前保序，跨会话可并行；Core 准入上限为全局 128 条、每会话 16 条、32 个活跃会话及 16 MiB 估算内容预算。超限拒绝新消息。插件仍需管理自己的业务队列。
- `sdk.Configurable`／`sdk.ConfigurationActions` 提供插件声明的配置表单和管理操作。配置读取／保存期限 180 秒，管理操作期限 660 秒；插件须响应更早的取消和关闭期限。
- `sdk.DocumentEditable` 独立提供文本／JSON 文档编辑。插件声明选择字段、比较修订并校验、持久化正文；Core 不编辑任意路径，不解析业务 JSON。文档最多 8 MiB，选择参数最多 16 KiB，全局两路请求，操作期限 180 秒。冲突保留编辑草稿，需重新读取核对。

SDK API 仍为 1；新增公开类型会改变 Go 共享包身份。本次更新后须用公开仓库的同份 SDK 和匹配构建设置重建 Core 与全部插件；私人源码树与公开模块的二进制不能混用。更新内容见 [CHANGELOG.md](CHANGELOG.md)。

## 许可证与协议参考

项目自有代码采用 [Apache-2.0](LICENSE)，第三方依赖保留各自许可证，见 [第三方声明](THIRD_PARTY_NOTICES.md)。QQ／NapCat 独立运行，不随仓库分发。

协议参考：[OneBot 11](https://github.com/botuniverse/onebot-11)、[NapCat 文档](https://napneko.github.io/)。
