# moesekai-masterdata-sync 设计文档

> 状态：N1（cn 跑通一轮）已完成，见 §13；其余设计中
> 代码仓库：`StarMoe-org/moesekai-masterdata-sync`（公开，尚未创建）
> 数据仓库：`StarMoe-org/moesekai-masterdata-<region>`（公开，尚未创建）
> 最后更新：2026-11-11
> 旧设计（以 workflow 产出身份数据为前提）已被本文取代，不再收录。

## 1. 背景与目标

现在下游依赖 Haruki 的 masterdata 仓库，经常延迟。本服务自己采集 Project SEKAI 各服的 masterdata，发布到自有的公开数据仓库；目录结构和 JSON 内容与 Haruki 一致，下游换一个仓库地址就能切过来。

它和 **pjsk-meta-builder**（另一个项目，C#）配合：builder 负责一切和安装包有关的事，把每个版本的 App 元数据（appVersion、appHash、密钥、Nuverse 结构文件）发布到公开仓库 `StarMoe-org/moesekai-app-meta`；本服务只负责游戏 API 和 master。

**第一版范围**：jp、cn。en / hmt / kr 以后加（en 走 jp 的流程，hmt / kr 走 cn 的流程）。

**不做的事**

- 不轮询安装包来源（官网、QooApp、Google Play），不下载安装包，不提取元数据——这些是 builder 的事。
- 不下载、不解包素材资源。
- 不通知下游（下游轮询 git 或用 GitHub 的 push webhook）。
- 不做多副本选主。

## 2. 已定决策

| # | 主题 | 决策 |
| --- | --- | --- |
| S1 | 语言 | Go |
| S2 | 代码仓库 | `StarMoe-org/moesekai-masterdata-sync`，**公开**（2026-11-11 由私有改定：已有不少公开项目包含 jp 注册流程，私有带来的保护有限）。代码里不放任何密钥、账号或 uid，全部来自 Secret 或 app-meta |
| S3 | 构建与更新 | **已定（2026-11-11 改）**：和 pjsk-meta-builder 一样用 StarMoe 标准 `deploy.yml`：push 到 main → GitHub Actions 构建 `ghcr.io/starmoe-org/moesekai-masterdata-sync:<sha12>` → 用 `CNB_TOKEN` 改 infra → Ferry 滚动更新；首次创建用 ssh + `kubectl create` |
| S4 | 发布方式 | **本服务自己用 git 推送**到 `moesekai-masterdata-<region>`（不检出工作目录的部分克隆放在 emptyDir，提交、推送；§7.1）。服务不对公网开放 |
| S5 | GitHub 凭据 | 复用 GitHub App `moesekai-bot`，安装范围扩到各数据仓库；换 token 时只申请当前那一个仓库 |
| S6 | jp 账号 | 服务**自动注册**，写回 K8s Secret；服务有自己的 ServiceAccount + Role，只能读写这一个账号 Secret（和变量 Secret 分开） |
| S7 | 分工 | 安装包相关的全归 builder（含主动发现新安装包）；本服务只在游戏 API 报版本过期、或 master 和结构文件对不上时催 builder（§6） |
| S8 | 版本信号 | 作为拉 master 的副产物获得，不单独轮询：jp 登录时的 426；cn 免 token login 的 426 / `appVersionStatus` |
| S9 | 输出格式 | 与 Haruki 一致（字段名、值、目录结构），不追求逐字节一致 |
| S10 | 协议参数 | appVersion、appHash、AES 密钥、Nuverse 结构文件**都从 `moesekai-app-meta` 读**，不写在配置里；域名等其余参数在配置里，代码不写死 |
| S11 | 部署 | JP2，`apps` 命名空间，单副本，`strategy: Recreate`；和 builder 走集群内网，双向 HMAC |
| S12 | 写仓库的方式 | **不检出工作目录**（§7.1）：只拿目录树，按 git blob 哈希判断哪些文件变了，只写变化的 blob 再生成提交。仓库格式保持和 Haruki 一致，不另做压缩包（对外的压缩分发由下游 Moe_metadata_NEXT 负责） |

## 3. 总体结构

```
                     moesekai-app-meta（公开，builder 写）
                          │ latest.json / meta.json / moe-info.json / schema.json
                          ▼
 ┌──────────────── moesekai-masterdata-sync ─────────────────┐
 │ AppMeta（每区服当前版本的元数据，启动和收到回调时刷新）   │
 │ RegionWorker × N（每个区服一个 goroutine）                 │
 │   ├ jp：注册/登录 → 分片 master                            │──▶ git push ──▶ moesekai-masterdata-jp
 │   └ cn：免 token login → CDN master → 用结构文件还原       │──▶ git push ──▶ moesekai-masterdata-cn
 │ AccountStore（K8s Secret）                                 │
 │ HTTP：/healthz /readyz /metrics  POST /v1/app-meta/callback│◀── builder 回调（HMAC）
 └─────────────────────────────────────────────────────────────┘
          │ POST /v1/builds（HMAC，只在 §6 的情况下）
          ▼
   pjsk-meta-builder（JP2 内网）
```

### 3.1 包划分

| 包 | 职责 |
| --- | --- |
| `cmd/moesekai-sync` | 入口：`serve`、`once`（跑一轮就退出）、`decode`、`version` |
| `internal/appmeta` | 读 `moesekai-app-meta`：latest.json、meta.json、moe-info.json（0/1 位串解码）、schema.json；校验 sha256 |
| `internal/crypto`、`internal/msgpack`、`internal/jsonout` | AES-128-CBC；MessagePack 解码与编码；Python 兼容的 JSON 输出 |
| `internal/sega` | CP 服客户端（jp）：game-version、CloudFront cookie、注册、登录、分片下载；账号存储接口和文件实现 |
| `internal/nuverse` | Nuverse 客户端（cn）：免 token login、CDN 下载 |
| `internal/schema` | 按结构文件还原 Nuverse master、compact 表展开 |
| `internal/master` | 写出表和版本文件、删除保护、版本号比较 |
| `internal/gitrepo`、`internal/publish` | 不检出工作目录的 git 发布（§7.1）；本地目录 / git 两种写入目标 |
| `internal/ghapp` | GitHub App installation token |
| `internal/syncer` | 一轮同步（once 和 serve 共用），错误分成过期 / 维护 / 对不上结构文件 |
| `internal/worker` | 每个区服的主循环：调度、抖动、退避、等 builder（§6、O4） |
| `internal/builder`、`internal/hmacauth` | 调 builder 的 `POST /v1/builds`、回调格式；双向 HMAC |
| `internal/kube` | 账号存储的 K8s Secret 实现（只用 get / patch 两个请求，不引入 client-go） |
| `internal/httpapi` | `/healthz`、`/readyz`、`/metrics`、`POST /v1/app-meta/callback` |

## 4. 输入：moesekai-app-meta

格式定义见 builder 设计文档 §4。本服务用到：

| 文件 | 用途 |
| --- | --- |
| `<region>/latest.json` | 当前版本：`appVersion`、`appHash`、`unityVersion`、`package.versionName` / `versionCode`、`files` 里各文件的 sha256 |
| `<region>/<appVersion>/moe-info.json` | `[0]` API 密钥：jp 的 API 和 master 分片、cn 的 API 和 CDN master 都用它 |
| `<region>/<appVersion>/schema.json` | 只有 Nuverse 服：表名 → 类、字段下标 → 字段名和类型 |

- 读取用 raw 地址或 GitHub API（公开仓库，不需要 token）；按 `files` 里的 sha256 校验，对不上就不采用。
- 只前进：新的 latest 的 appVersion 比当前的小，忽略。
- **刷新时机**：启动时；收到 builder 回调时；§6 里本服务发现版本过期时。不定时轮询。
- **cn 请求用的版本号**：Nuverse 客户端实际发送的 `X-App-Version` 是四段 `{versionName}.{versionCode}`（cn 6.4.1 为 `6.4.1.4647`），由 `package` 字段拼出；服务器只看前三段。jp 直接用 `appVersion`（三段）。

## 5. 各区服的拉取流程

### 5.1 jp（CP 服）——需要账号

```
GET  https://game-version.<domain>/{appVersion}/{appHash}     → profile、domain、assetbundleHostHash
POST https://issue.<domain>/api/signature                     → CloudFront Set-Cookie（转成 Cookie 请求头）
PUT  https://{domain}/api/user/{userId}/auth?refreshUpdatedResources=False
     请求体：msgpack{credential, deviceId, authTriggerType:"normal"}，用 [0] 密钥加密
     → sessionToken、dataVersion、assetVersion、assetHash、multiPlayVersion、appVersionStatus、
       removeAssetVersion、suiteMasterSplitPath[]
dataVersion 比数据仓库里的新：
GET  https://{domain}/api/{suiteMasterSplitPath[i]}            → 逐个分片流式解密、解码、按表写出
```

- 请求头：`X-App-Version`、`X-App-Hash`、`X-Data-Version`、`X-Asset-Version`、`X-Unity-Version`、`X-Platform`、`X-DeviceModel`、`X-OperatingSystem`、`X-Install-Id`、`X-Request-Id`、`X-Session-Token`。
- 状态码：403 → 重新拿 cookie、重新登录一次；**426 → 版本过期（§6）**；503 → 维护，退避。
- 2026-11-11 实测（jp 7.1.0，本机）：game-version 返回 `profile=production`、`domain`、`assetbundleHostHash`；`issue/api/signature` 的响应体是 `{}`，Set-Cookie 里有 `CloudFront-Policy` / `-Signature` / `-Key-Pair-Id` 三项，拼成 Cookie 后 API 才不再返回 CloudFront 的 403。
- **game-version 对 JP2 的 IP 返回 CloudFront 403**（2026-11-11 实测；本机正常，issue 和游戏 API 在 JP2 上都能访问）。game-version 只用来拿 API 域名，正式服域名固定为 `production-game-api.sekai.colorfulpalette.org`，所以 game-version 返回 HTTP 错误时记告警、改用这个默认域名（`sega.Config.DefaultAPIDomain`）。JP2 节点上实测回退后登录、下载 417 张表正常。
- **`GET /api/system` 只要 cookie、不用登录**，返回全部 appVersion（当前 100 条）及各自的 `appVersionStatus`、`assetVersion`、`multiPlayVersion`；目前只有 7.1.0 是 `available`。新版本上线时会出现在这里，可作为 jp 的新版本信号（比登录便宜），交给 builder M6 参考。
- Haruki jp 仓库的输出与 cn 不同：版本文件字段顺序为 appHash, systemProfile, appVersion, multiPlayVersion, assetVersion, appVersionStatus, dataVersion, assetHash, removeAssetVersion, cdnVersion（恒为 0）；表内键保持服务器顺序，不排序；另有 `compactCards` / `compactGachas`，**不是服务器的数据**，是 Haruki-Sekai-API 自己生成的派生文件（`cards` / `gachas` 里的 `cardParameters` / `gachaDetails` 改成按列存放，给 Haruki-Toolbox 减小下载体积），见 O10。实现见 `master.LayoutSega`；对照结果见 §15。
- 本机用 `MOESEKAI_JP_ACCOUNT_FILE`（JSON，0600）保存账号；`once` 只有加 `--register` 才会注册。
- **自动注册**：账号 Secret 里没有账号时，`POST /api/user`（`{platform, deviceModel, operatingSystem}`）→ `userId`、`credential`；**先写入 Secret 成功，再使用**。账号失效（登录 401/403 重试后仍失败）时报告警，不自动换号。

### 5.2 cn（Nuverse 服）——不需要账号

> **临时方案。** 下面的免 token login 是服务端鉴权的缺陷，已报告官方（2026-11-11）。官方修复后改为用一个固定的、已登录的账号拉取（带会话凭据），凭据放在 Secret；届时 cn 的流程向 jp 靠拢（登录 → 拿会话 → 请求）。在那之前 `nuverse.Client.Login` 仍按下面的方式工作；修复上线后免 token login 会开始失败（具体状态码未知），应报警而不是重试。

2026-11-11 对 cn 6.4.1 安装包静态分析并实测确认：

```
POST https://mkcn-prod-public-60001-1.dailygn.com/api/user/{已存在账号的 uid}/login
  请求头：Content-Type / Accept: application/octet-stream、X-App-Version（四段）、X-App-Hash、
          X-Platform、X-Install-Id、X-Request-Id；不带 X-Session-Token
  请求体：空明文，用 [0] 密钥 AES-128-CBC 加密（16 字节）
  → 200，用 [0] 解密：msgpack{appVersion, appVersionStatus, dataVersion, assetVersion, cdnVersion, multiPlayVersion, configs…}
cdnVersion 比数据仓库里的新：
GET  {CDN}/MasterData/60001/master-data-{cdnVersion}.info     → [0] 密钥解密 → msgpack → 按结构文件还原 → 写出
```

- **uid 必须是已存在的账号**（只要 id，不要它的 token）。随机生成的 id（Unix 毫秒 << 22 + 随机低位，形式与真实 id 相同）实测返回 500。配置为 `MOESEKAI_CN_UID`（§10 O6）。
- CDN 是客户端 `AssetBundleManager.GetMasterDataUrl` 的拼法：`CDNList[随机].Replace("AssetBundle","MasterData") + serverId + "/master-data-{cdnVersion}.info"`。安装包里写死 4 个：`https://lf{3,6,9,26}-mkcncdn-tos.dailygn.com/obj/sf-game-lf/gdl_app_5236/AssetBundle/`；随机选一个，失败换下一个。四个地址 ETag 相同。
- 实测：当前版本 `6.4.1.4647` 和 `6.4.0.4424` 都返回 `available`，`dataVersion`（6.4.1.6）和 `cdnVersion`（186）相同；`6.0.0.x` 返回 **426**、`not_available`，响应用 [1] 密钥加密。**响应里的 `appVersion` 只是回显请求的版本，不是最新版本。**
- 所以这个接口只能发现「手上的版本被淘汰了」，发现不了「新版本已上架」——后者由 builder 负责（§6）。
- `api/system` 不检查版本、只返回 `serverDate`，不用。
- 版本文件里的 `dataVersion`、`assetVersion`、`cdnVersion` 直接取 login 响应。
- 客户端拼地址前会检查 IFix 热修复补丁；将来官方用补丁改了规则，安装包里看不出来，表现为 CDN 404。连续 404 而 login 的 cdnVersion 已前进时报告警，不只是重试。

### 5.3 还原 Nuverse master

N1 用 cn 186 和 Haruki sc 仓库逐表对照后定下的规则（与 Haruki 一致）：

- `.info` 解密后是一个 msgpack map，顶层键是表名（cn 186：424 个）。
- **按下标序列化的类**（`[Key(n)]`，cn 74 张表）：行是数组，按 schema 的 `key` 还原成对象，字段名用 `name`，**值为 null 的字段省略**；字段顺序为下标顺序。
- **按名字序列化的类**（`[Key("x")]`，cn 352 张表）：行是 map，**键换成 schema 的字段名**（如 `receiveRewardEndAt` → `rewardReceiveEndAt`），值按字段类型递归还原（嵌套的数组对象也要还原），然后**按键排序**；schema 里没有的键原样保留。
- 嵌套的类、类数组、`Dictionary` 递归还原；`pair` / 枚举 / 无法描述的类型原样输出。
- **`compact*` 表**：原样输出（按列：`__ENUM__` + 各列数组），**另外展开成一张普通表**（`compactBondsRewards` → `bondsRewards`）：每个下标一行，列按原顺序，`__ENUM__` 里有的列把下标换成取值，null 保留。cn 有 8 张。
- 校验：类型不符（如 int 位置出现 string）→ 本区服本轮失败、不写任何文件，并按 §6 催 builder。行比类**多出的尾列**丢弃并告警（cn 的 `cardCostume3ds` 多一列 bool，客户端和 Haruki 都不读）。
- schema 里没有的顶层键（cn 186 有 9 个）：原样输出并告警。

## 6. 和 builder 的配合

正常情况下 builder 自己先发现新安装包：处理、发布到 app-meta，再回调本服务（`POST /v1/app-meta/callback`，HMAC 签名，格式见 builder 设计文档 §7.2）。本服务收到 `advanced` 后刷新 app-meta，下一轮请求就换成新版本。

本服务先发现的情况：

| 信号 | 区服 | 处理 |
| --- | --- | --- |
| 登录返回 426 / `appVersionStatus: not_available` | jp、cn | 先刷新 app-meta；有更新的版本就直接换上重试。没有就 `POST /v1/builds`（不带 expectedVersionCode）；409 表示 builder 已在处理同一区服，等回调 |
| master 和结构文件对不上（§5.3） | cn | 同上；同时这个区服暂停发布 |

- 等待期间该区服暂停，按退避节奏（1 分钟起，翻倍，最长 30 分钟）重新检查；收到回调立即恢复。
- 回调结果为 `failed`：按 `error.kind` 处理——`network` / `not_ready` 过一段时间再发；`data` / `config` / `internal` 报告警、不自动重发。
- 签名：`X-Moe-Timestamp`、`X-Moe-Signature: sha256=hex(HMAC_SHA256(secret, ts + "." + body))`，时间差超过 5 分钟拒绝；和 builder 共用 `MOE_HMAC_SECRET`。

## 7. 输出格式

每个区服一个数据仓库 `StarMoe-org/moesekai-masterdata-<region>`，目录与 Haruki 相同：

```
master/<table>.json
versions/<dataVersion>.json
versions/current_version.json
```

- JSON：两空格缩进，`": "` 和 `,` 分隔，非 ASCII 不转义，保留字段顺序；浮点按最短表示；二进制 / ext 输出 base64；整数 map key 转字符串。写文件先写临时文件再改名，内容没变不动文件。
- 版本文件字段顺序：`appHash, systemProfile, appVersion, multiPlayVersion, assetVersion, appVersionStatus, dataVersion, assetHash, removeAssetVersion, cdnVersion`，没有的省略（Haruki 的 jp / cn 实际各缺一些字段，对照后定）。
- 提交信息：`Sekai master data version <dataVersion>`；提交者是 `moesekai-bot[bot]`。
- **下游**：StarMoe 的 `Moe_metadata_NEXT` 直接 clone Haruki 的 5 个仓库（区服代号 `jp/cn/tw/kr/en`），做 brotli / gzip 预压缩后对外提供，并生成 `snowy_costumes.json`、`moe_costume.json` 两张派生表（读 `costume3ds`、`cardCostume3ds`、`costume3dShopItems`、`costume3dGroups`、`costume3dColors`、`costume3dShopItemCosts`，其中后几张是 compact 展开表）。切换数据源只需改它的仓库地址，所以目录结构、compact 展开表都必须保持；派生表留在它那边生成，不放进数据仓库。
- **删除保护**：一张表连续两次完整拉取都缺失才删除；本轮表数少于现有的 75% 时不删除；一次最多删 10 个。

### 7.1 写仓库（不检出工作目录）

cn 一份完整输出约 230 MB（432 个文件，其中 compact 展开表 82 MB），但每次更新通常只改几个文件（185→186 改了 4 个，diff 压缩后约 70 KB）。所以不检出工作目录：

1. 第一次运行时 `git init --bare`，配成部分克隆（`extensions.partialClone=origin`、`remote.origin.partialCloneFilter=blob:none`），之后每轮 `git fetch --filter=blob:none --depth=1`，只拿提交和目录树；`git ls-tree -r -z` 得到每个文件的 blob 哈希。远端还没有分支（空仓库）时从空树开始。
2. 每张表生成 JSON 后立即算 git blob 哈希（`sha1("blob <长度>\0" + 内容)`），和树里的比较；相同就丢掉，不同才由 Go 直接写成松散对象（zlib）。表生成完就释放，不在内存里攒整份 master。
3. Go 按「路径 → 对象名」自己写出树对象（只有变化的子树产生新对象），`git commit-tree` 生成提交，`git push --no-thin`；被拒（非快进）时重新 fetch，在新的 HEAD 上重算（只读树，不需要合并），最多 3 次。如果某个文件当初因「没变」没写对象、而远端在这期间改了它，直接报错，下一轮重新生成。
4. 删除保护（§7）按「这次没有、树里有」的文件计算。
5. 读 `versions/current_version.json` 是唯一按需下载 blob 的地方（只下这一个对象）；其余 git 命令都带 `GIT_NO_LAZY_FETCH=1`，任何意外需要旧内容的操作会直接失败，而不是悄悄下载整份数据。
6. 推送必须 `--no-thin`：thin pack 会拿远端已有的旧内容做差量基准，需要读本地没有的旧 blob。
7. token 只通过 `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_0` / `GIT_CONFIG_VALUE_0` 环境变量传 `http.extraHeader`，不出现在命令行参数和 `.git/config` 里。

效果：磁盘只放 git 对象（几 MB），内存峰值只剩一份解码后的 master，O9 一并解决。

## 8. 状态

| 状态 | 位置 | 丢失的影响 |
| --- | --- | --- |
| 数据仓库副本 | emptyDir，只有提交和目录树（§7.1） | 重新拉取（几百 KB） |
| 当前版本 | 数据仓库的 `versions/current_version.json` | 跟着仓库走，不会丢 |
| app 元数据 | 内存，启动时从 app-meta 读 | 重新读 |
| jp 账号 | K8s Secret `moesekai-masterdata-sync-accounts` | 必须持久化（S6） |
| 会话、cookie、待删列表、退避状态 | 内存 | 重新登录；推迟一轮删除 |

## 9. 配置与部署

| 配置 | 来源 | 说明 |
| --- | --- | --- |
| `MOE_HMAC_SECRET`（或 `_FILE`） | Secret | 与 builder 共用 |
| `GITHUB_APP_ID`、`GITHUB_APP_PRIVATE_KEY` | Secret | `moesekai-bot` |
| `BUILDER_URL` | 配置 | 默认 `http://pjsk-meta-builder.apps.svc.cluster.local:8080` |
| `MOESEKAI_REGIONS` | 配置 | 默认 `jp,cn` |
| `MOESEKAI_POLL_<REGION>` | 配置 | 拉 master 的间隔（§10 O4） |
| 各区服的域名、设备信息 | 配置 | jp 的 game-version / issue 域名，cn 的 API 地址和 CDN 列表；示例配置里用占位符 |
| `ACCOUNT_SECRET_NAME` | 配置 | 默认 `moesekai-masterdata-sync-accounts` |

- Deployment：单副本、`Recreate`；emptyDir 申请 2Gi 临时存储；非 root、只读根文件系统；内存 request / limit 必填（StarMoe 约定）。
- ServiceAccount + Role：只允许 `get` / `update` / `patch` 账号 Secret 这一个对象（`resourceNames`）。
- Service：ClusterIP，只给 builder 回调用，不建 Ingress。
- 镜像：Alpine + `git` + `ca-certificates`，`ghcr.io/starmoe-org/moesekai-masterdata-sync`（S3）。完整的变量表和首次创建步骤见 `deploy/README.md`。

## 10. 未决项

| # | 问题 | 当前打算 |
| --- | --- | --- |
| O1 | jp 注册与登录的完整请求细节（设备字段、请求头取值） | 对照 jp 7.1.0 安装包和 sssekai 确认；先在本机跑 `register` + `once` |
| O2 | ~~`compact*` 表展开规则、Haruki 多出的文件~~ | **已解决（N1）**：多出的 8 个文件就是 compact 表展开的结果，规则见 §5.3 |
| O3 | ~~数据仓库与 App 安装范围~~ | **已定（2026-11-11）**：五个公开仓库 `StarMoe-org/moesekai-masterdata-{jp,cn,en,hmt,kr}` 已建（区服代码与 moesekai-app-meta 一致，繁中服用 `hmt`），`moesekai-bot` 已加装到这五个仓库；en / hmt / kr 暂时空着 |
| O4 | ~~拉 master 的间隔~~ | **已定（2026-11-11）**：jp、cn 默认都是 **5 分钟**（`MOESEKAI_POLL_JP` / `MOESEKAI_POLL_CN` 可调），±10% 抖动；网络错误、5xx 指数退避（1 分钟起，最长 30 分钟）；503 维护固定 5 分钟后再试；426 / `not_available` 暂停该区服，转 §6 刷新 app-meta、请求 builder；jp 的 cookie 跨轮复用，403 时才重新获取 |
| O5 | ~~首次导入~~ | **已定（2026-11-11）**：数据仓库从空仓库开始，第一次运行提交当前完整的 master 和版本文件，不导入 Haruki 的历史 |
| O6 | ~~cn login 用哪个账号的 uid~~ | **已定（2026-11-11）**：用户自己的国服账号 uid（只用 id，不用 token），放在服务的 Secret `MOESEKAI_CN_UID`；值不进仓库、不写日志。被风控时影响的是这个账号，需要换号时只改这个值。官方修复免 token login 后改用固定的已登录账号（§5.2），凭据同样只放 Secret |
| O7 | ~~`billingShopItemRandomBoxGroups`~~ | **已定（2026-11-12）：保持对象格式**（`{id, randomBoxGroupId, resourceBoxId, rate}`），与其他表一致。Haruki（sc / tc / kr）和 Sekai-World 的 sekai-client 输出无字段名的数组，只是因为它们的结构生成器拿不到这个类（类名不带 Master 前缀）的字段名，不是有意为之；值完全相同。只有 Nuverse 系的服有这张表，StarMoe 下没有项目使用 |
| O8 | ~~字段顺序~~ | **已定（2026-11-12）：不要求与 Haruki 一致。** cn 有 24 张表 Haruki 用类里的声明顺序，我们按字段名排序（schema.json 不记录声明顺序），值相同；不为此改 builder 的 schema.json 格式 |
| O10 | ~~jp 的 `compactCards` / `compactGachas`~~ | **已定（2026-11-11）：不生成。** 这是 Haruki 给自家工具做的体积优化，不是游戏数据，StarMoe 下没有项目使用；对外压缩分发由下游负责（S12）。我们的 jp 输出只有服务器给出的表 |
| O11 | jp 版本文件的 `multiPlayVersion` | Haruki 的 208 个版本文件全是 `miku`（写死的旧值），服务器现在返回 `kaito`；我们用服务器的真实值。Moebot-NEXT 只解析、不依赖具体值 |
| O9 | ~~内存~~ | **已定（S12）**：按 §7.1 逐表生成、算哈希、只写变化的 blob；实现时测峰值 |

## 11. 测试

- 单元：AES 往返、msgpack 流式解码、0/1 位串解码、浮点格式、版本比较、schema 还原（含类型不符）、compact 展开、删除保护、HMAC。
- 集成：`httptest` 模拟 jp（game-version / signature / 注册 / auth / 分片，含 403、426、503）、cn（login、CDN、404 换地址、426）和 builder（202 / 409 / 回调）。
- git：临时目录里的裸仓库（`uploadpack.allowFilter=true`）当远端，覆盖空仓库首次提交、内容不变不提交、只写变化的对象、删除、推送被拒后在新 HEAD 上重算、按需下载只发生在 `ReadFile`；每步后对远端 `git fsck --strict`。
- 回归：用 Haruki 仓库的真实文件对照字段名和值（不比字节）。

## 12. 里程碑

| 里程碑 | 内容 | 完成标志 |
| --- | --- | --- |
| N1 cn 一轮 | 配置、app-meta 读取、cn login + CDN + 还原、规范 JSON、`once` 写到本地目录 | 本机 `once` 产出的 cn master 与 Haruki sc 仓库逐表对照一致（除 O2 已知差异） |
| N2 发布 | git 发布、GitHub App token、删除保护、版本文件 | 推到本地裸仓库；对真实数据仓库 dry-run |
| N3 jp | 注册、登录、分片、AccountStore | 本机注册一个账号并跑通 `once` |
| N4 服务 | `serve`、调度与退避、与 builder 的配合（§6）、健康检查与指标、镜像、部署 JP2 | JP2 上两区服持续同步；模拟 426 能触发 builder 并在回调后恢复 |

## 13. N1 结果（2026-11-11）

- 代码：Go 1.27，无第三方依赖。`internal/{msgpack,crypto,jsonout,appmeta,schema,nuverse,master}`，命令 `decode`（离线）和 `once`（cn）。
- `once --region cn`：读 app-meta 的 cn/6.4.1 → 免 token login（`6.4.1.4647`）得到 cdnVersion 186 → 从 CDN 下载 36MB → 解密、还原、写出 432 个表文件和版本文件，约 3 秒；再跑一次判为没有新 master，不写任何文件。
- 与 Haruki sc 仓库（同为 cdnVersion 186）对照：文件集合完全相同（432 个）；**407 个逐字节相同**，24 个只差字段顺序（O8），1 个值不同（O7）；`versions/current_version.json` 逐字节相同。
- JSON 输出按 Python `json.dumps(indent=2, ensure_ascii=False)` 实现，浮点数格式用 Python 的结果做了测试向量。
- 单元测试覆盖 msgpack 解码与编码、AES 与 0/1 位串、JSON 格式、schema 还原的各条规则、compact 展开、写文件与版本文件、login（200 / 426 用 [1] 密钥 / 503 / 500）、CDN 换地址与全部 404、app-meta 的 sha256 校验。

## 14. N2 结果（2026-11-11）

- 代码：新增 `internal/{gitrepo,ghapp,publish}` 和 `master/guard.go`；`master` 改为写入 `Sink` 接口。`once` 支持 `--out <目录>` 或 `--repo <地址> [--work <目录>]`；地址是 `https://github.com/<owner>/<repo>` 时用 `GITHUB_APP_ID` / `GITHUB_APP_PRIVATE_KEY`（或 `_FILE`）换只针对该仓库的 installation token。提交者 `moesekai-bot[bot]`，提交信息 `Sekai master data version <dataVersion>`。
- 单次运行（`once`）不删除任何表：删除保护要求「连续两次缺失」，只在 `serve` 里生效。
- 真实 cn 数据推到本地空裸仓库：第一次提交 432 个表 + 2 个版本文件，全程约 10 秒，仓库 12 MB；clone 出来与 N1 的本地输出逐字节一致。再跑一次判为没有新 master；换一个全新工作目录 `--force` 重写全部表，432 张全部判为没变、不产生提交，工作目录里只有树对象和 `current_version.json` 一个 blob（约 40 KB）。
- 内存峰值约 1.3 GB（RSS，`--force` 全量还原时），主要是解码后的整份 master；部署时 limit 先按 2 Gi 设，N4 再看是否需要优化。
- 推到 GitHub `StarMoe-org/moesekai-masterdata-cn`：换到只对该仓库有效的 installation token，第一次提交约 14 秒（RSS 峰值约 1.1 GB）；再跑一次判为没有新 master。从 GitHub 克隆回来与本地输出逐字节一致；工作目录的 `config` 里没有任何凭据。

## 15. N3 结果（2026-11-11）

- 代码：新增 `internal/sega`（game-version、CloudFront cookie、注册、登录、分片下载、账号存储），`master.LayoutSega`、`master.CompareVersions`；`once --region jp [--register]`。
- 本机实测 jp 7.1.0：注册一个新账号（保存在本机账号文件，0600）→ 登录得到 dataVersion 7.1.0.60、7 个分片 → 写出 417 张表和版本文件，约 39 秒（分片下载占 30 秒），RSS 峰值约 650 MB；再跑一次判为没有新 master。
- 与 Haruki jp 仓库（同为 7.1.0.60）对照：**417 张表全部逐字节相同**；Haruki 多 2 个派生文件（O10，决定不生成）；版本文件只差 `multiPlayVersion`（O11），其余字段和格式一致。
- 推到 GitHub `StarMoe-org/moesekai-masterdata-jp`：第一次提交约 42 秒；再跑一次判为没有新 master；克隆回来与本地输出逐字节一致，工作目录 `config` 里没有凭据。
- 模拟服务器测试覆盖：注册并保存、读已保存的账号、403 后重新拿 cookie 重试、426 / 503 / 500、分片下载、服务器换发 session token、错误信息不含 userId。

## 16. N4 进度（2026-11-11）

- 代码：`serve` 命令和 §3.1 里新增的 `syncer`、`worker`、`builder`、`hmacauth`、`kube`、`httpapi`；`gitrepo` 增加 `ReadFile` 缓存（按 blob 对象名）和超过 512MB 清空本地副本。两个区服共用一把锁，同一时间只有一个区服在跑一轮。jp 的 cookie 跨轮复用。
- 部署文件：`Dockerfile`（golang:1.27-alpine 交叉编译 → alpine:3.22 + git 2.49，非 root）、`deploy/k8s.yaml`（ServiceAccount、空的账号 Secret、只能 get / patch 这一个 Secret 的 Role、Deployment、Service）、`deploy/README.md`；`deploy.yml` 与 StarMoe 模板逐字节相同，仓库能读到组织级的 `CNB_TOKEN`。
- 本机演练：`serve` 推到两个本地空裸仓库（间隔 30 秒、不接 builder），首轮 jp、cn 依次全量写出并提交，之后每轮都判为没有新 master；jp 第二轮起只发登录请求；签名回调 204、错误签名 401；`/healthz`、`/metrics` 输出正确；SIGTERM 后 3 秒内退出，全程无错误日志。
- 单元测试：worker（正常间隔、退避翻倍到 30 分钟、维护、过期后请求 builder 并等待、可重试 / 不可重试的回调、app-meta 已有新版本时直接换上）、httpapi、hmacauth（与 Python 独立算出的向量对照）、kube（模拟 API server）。
- 镜像：GitHub Actions 构建成功（`9432dd704c53`），GHCR 可匿名拉取，Ferry 通知成功。
- JP2 首次部署（ssh + `kubectl create`）：账号 Secret 先写入本机已注册的 jp 账号，再建变量 Secret 和其余对象。cn 第一轮即正常（判为没有新 master）；jp 因 game-version 403 失败，按上一条修复。
- 修复（`720597c`）推送后由 Ferry 自动更新到 `720597ceacb2`：jp 回退到默认域名后登录正常，jp、cn 都判为没有新 master，`/healthz` 两个区服都没有失败；空闲时内存约 9Mi。账号 Secret 里的 jp 账号直接使用，没有重新注册。
- 还没有在线上经历过真实的新 master 和版本过期；builder 的主动发现与 `SYNC_CALLBACK_URL`（builder M6）尚未实现，目前只有本服务发起的请求会收到回调。

