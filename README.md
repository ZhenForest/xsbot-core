# xsbot-core

单账号、轻量的 Go QQ Bot Core。通过一条 OneBot 11 通用正向 WebSocket 连接独立运行的协议端；业务由可信 Go 插件实现。

Core 包含严格消息解析、Rule 路由、媒体资源管理、插件生命周期、HTTPS 管理页和日志。公开目录仅包含生产源码、SDK、构建依赖及许可证；没有业务插件、测试代码、测试脚本、离线表情名称表或私人部署资料。

## 构建

正式运行要求 **Linux、Go 1.27.3、CGO_ENABLED=1**。依赖已包含在 vendor 中：

```sh
CGO_ENABLED=1 go build -mod=vendor -trimpath -o build/bot-core ./cmd/bot-core
```

模块路径：`github.com/ZhenForest/xsbot-core`；公开接口见 [sdk/types.go](sdk/types.go) 和 [sdk/configuration.go](sdk/configuration.go)。

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

## 许可证与协议参考

项目自有代码采用 [Apache-2.0](LICENSE)，第三方依赖保留各自许可证，见 [第三方声明](THIRD_PARTY_NOTICES.md)。QQ／NapCat 独立运行，不随仓库分发。

协议参考：[OneBot 11](https://github.com/botuniverse/onebot-11)、[NapCat 文档](https://napneko.github.io/)。
