# 自有渠道维护

维护仓库：[cgl1307929582-prog/sub2api](https://github.com/cgl1307929582-prog/sub2api)。
上游仓库：[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)。

## 分支与同步

`origin` 指向自有仓库，`upstream` 指向官方。`main` 接收完成验证的功能；每个渠道在独立分支实现，例如 `codex/droid-factory`。同步官方更新时建立单独的同步分支，合并 `upstream/main`，解决冲突并运行回归后再合入自有 `main`。保持官方提交历史和许可证，避免复制整份旧目录覆盖新基线。

## 新增渠道的入口

1. 在 `backend/internal/domain/platforms.go` 登记平台，在 `internal/service/provider_profile.go` 声明接入模式、原生协议端点和模型分流规则。
2. 将供应商的认证、请求准备、错误与额度行为放在对应适配文件中。管理端连接测试与实际转发使用相同出站逻辑，共用鉴权、分组、调度、并发及计费。
3. 在 profile 中维护候选模型；不要把静态列表描述成实时账号权限。上游模型发现、余额查询或媒体能力未验证时不启用。
4. 更新前端平台目录：在 backend 运行 `UPDATE_PLATFORM_CATALOG=1 go test -tags unit ./internal/service -run TestFrontendBuiltinPlatformCatalogInSync`。保留生成目录与后端一致的检查。
5. 验证原生请求、跨协议转换、流式终态、工具往返、取消、错误换号和 usage 记录。真实调用使用独立测试数据及仓库外的凭据。

首个适配见 [DROID_FACTORY.md](DROID_FACTORY.md)。后续渠道沿用现有 profile 和协议转换能力；差异行为保持在适配层，避免增加重复网关。

## 发布边界

当前变更提供源码接入，尚未发布自有镜像或正式版本。验证二进制使用 `BuildType=source`。官方的一键更新源仍在 `backend/internal/service/update_service.go` 指向 `Wei-Shaw/sub2api`；发布自有版本之前需要单独适配更新来源和回滚来源，并验证自有 GitHub Releases / GHCR 产物。不要用官方发布包覆盖包含自有渠道的构建。

API Key、运行配置、测试数据库与真实请求原始日志保存在仓库外。公开验证记录仅保留模型、协议、成功状态与汇总用量。
