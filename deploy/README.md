# 部署

镜像：`ghcr.io/starmoe-org/moesekai-masterdata-sync`。main 分支由 `deploy.yml` 构建，标签为 12 位 commit sha 和 `main`（amd64）；`v*` 标签由 `ci.yml` 构建版本号和 `latest`（amd64 + arm64）。
默认命令 `serve`，监听 `PORT`（默认 8080），只往 `/work`（数据仓库的本地副本）和 `/tmp` 写。

## 必需的 Secret（`moesekai-masterdata-sync-env`）

| 变量 | 说明 |
| --- | --- |
| `MOE_HMAC_SECRET` | 与 pjsk-meta-builder 共用，至少 32 个字符 |
| `GITHUB_APP_ID`、`GITHUB_APP_PRIVATE_KEY` | GitHub App `moesekai-bot`（已安装到 `moesekai-masterdata-*`）；token 只申请当前要写的那一个仓库 |
| `MOESEKAI_CN_UID` | cn login 用的已存在账号 id（只要 id，不要 token） |

`GITHUB_APP_PRIVATE_KEY` 可以填原样 PEM、把换行写成 `\n` 的 PEM，或整个 PEM 文件的 base64。

## 可选配置

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MOESEKAI_REGIONS` | `jp,cn`（线上 `jp,cn,en`） | 同步哪些区服（可选 jp en cn hmt kr） |
| `MOESEKAI_POLL_<区服>` | `5m` | 多久查一次新 master（±10% 抖动，最少 30s） |
| `MOESEKAI_REPO_<REGION>` | `https://github.com/StarMoe-org/moesekai-masterdata-<region>` | 数据仓库 |
| `BUILDER_URL` | `http://pjsk-meta-builder.apps.svc.cluster.local:8080` | 填 `none` 时不请求 builder，只等 app-meta 更新 |
| `SYNC_CALLBACK_URL` | `http://moesekai-masterdata-sync.apps.svc.cluster.local:8080/v1/app-meta/callback` | 请求 builder 时告诉它的回调地址 |
| `ACCOUNT_SECRET_NAME` | `moesekai-masterdata-sync-accounts` | jp / en 账号所在的 Secret |
| `MOESEKAI_JP_REGISTER` | `true` | 账号 Secret 里没有 jp / en 账号时自动注册（先写入 Secret 并读回确认，再使用）；名字沿用旧的，对 en 同样生效 |
| `APP_META_BASE` | GitHub raw 上的 `moesekai-app-meta` | app 元数据来源 |

## 账号

jp / en 账号存在 Secret `moesekai-masterdata-sync-accounts` 的 `jp.json` / `en.json` 键里（`{userId, credential, installId, registeredAt}`）。en 账号是本机注册、跑通后导入的（2026-11-12）。服务的 ServiceAccount 只能 `get` / `patch` 这一个对象。

- 第一次启动时这个 Secret 是空的，服务会注册一个新账号并写进去。
- 要换号：删掉对应的键，重启服务；或者把已有账号的 JSON 写进去（`kubectl patch secret … --type merge`，数据从 stdin 传，不落文件）。
- 账号登录被拒（403，重新拿 cookie 后仍然失败）时只报错，不会自动换号。

## 资源

- 内存：cn 全量还原时 RSS 峰值约 1.3GB，jp 约 650MB；两个区服共用一把锁，不会同时下载、解码。上限 2Gi，`GOMEMLIMIT=1600MiB`。
- 临时存储：`/work` 只有目录树和本服务写过的对象，超过 512MB 自动清空重来。
- 单副本：两个副本会重复推送、重复注册账号。

## 出网

需要能访问：`api.github.com`、`github.com`、`raw.githubusercontent.com`，jp 的 `game-version.sekai.colorfulpalette.org`、`issue.sekai.colorfulpalette.org`、`production-game-api.sekai.colorfulpalette.org`，en 的 `game-version.sekai-en.com`、`n-production-game-api.sekai-en.com`，cn 的 `mkcn-prod-public-60001-1.dailygn.com`、`lf{3,6,9,26}-mkcncdn-tos.dailygn.com`。

## 查看状态

```sh
curl http://moesekai-masterdata-sync.apps.svc.cluster.local:8080/healthz   # 各区服版本、上次成功、最近错误
curl http://moesekai-masterdata-sync.apps.svc.cluster.local:8080/metrics   # Prometheus 格式
```

`POST /v1/app-meta/callback` 只接受 builder 的签名请求（设计文档 §6）。

## 首次创建（kubectl）

[k8s.yaml](k8s.yaml) 里是 ServiceAccount、空的账号 Secret、Role、RoleBinding、Deployment 和 Service，标签和命名按 Zeabur 面板的约定。

1. 准备 env 文件，一行一个 `KEY=VALUE`，不要加引号。`GITHUB_APP_PRIVATE_KEY` 填单行 base64。用完删掉这个文件。
2. 创建 Secret 和服务。**用 `create`，不要用 `apply`**：`apply` 会把 Secret 的值以明文留在 `last-applied-configuration` 注解里，以后再 `apply` 还会覆盖 Ferry 和面板的改动。

```sh
kubectl -n apps create secret generic moesekai-masterdata-sync-env --from-env-file=sync.env
kubectl -n apps label secret moesekai-masterdata-sync-env app.kubernetes.io/name=moesekai-masterdata-sync
# 先把 k8s.yaml 里的镜像标签换成一个已存在的 12 位 sha 标签
kubectl -n apps create -f k8s.yaml
kubectl -n apps rollout status deploy/moesekai-masterdata-sync
kubectl -n apps logs deploy/moesekai-masterdata-sync
```

也可以不落文件：在本机把这些行通过 ssh 管道传过去，服务器上用 `--from-env-file=/dev/stdin`。不要用 `--from-file` 建 Secret：末尾的换行会一起存进去。

## 自动更新

和 StarMoe 下其他项目一样，用标准模板 [`deploy.yml`](../.github/workflows/deploy.yml)，不要改它：推送到 main 后构建镜像，再用组织级的 `CNB_TOKEN` 运行 infra 仓库里的 `hack/bump.sh` 通知 Ferry。Ferry 只改镜像标签，不动环境变量和资源。

想临时固定某个版本：给 Deployment 加注解 `ferry.star.moe/auto-deploy: "false"`。
