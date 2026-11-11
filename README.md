# moesekai-masterdata-sync

Project SEKAI 各区服 master data 的同步服务：从游戏服务器拉取最新的 master，解密、还原成与 Haruki 一致的 JSON，推送到每个区服一个的公开数据仓库 `StarMoe-org/moesekai-masterdata-<region>`。

所需的 App 元数据（appVersion、appHash、密钥、Nuverse 结构文件）全部来自 [moesekai-app-meta](https://github.com/StarMoe-org/moesekai-app-meta)，由 pjsk-meta-builder 从安装包中提取；本仓库不包含任何密钥或账号。

设计与进度见 [docs/design.md](docs/design.md)。

## 状态

| 区服 | 状态 |
| --- | --- |
| cn | 可用（`once`） |
| jp | 可用（`once`；首次运行加 `--register` 注册账号） |
| en / hmt / kr | 未开始 |

## 用法

需要 Go 1.27+ 和 git 2.44+。

```sh
go build -o moesekai-sync ./cmd/moesekai-sync

# 写到本地目录
MOESEKAI_CN_UID_FILE=… ./moesekai-sync once --region cn --out ./out

# 提交并推送到数据仓库（不检出工作目录，只上传变化的文件）
MOESEKAI_CN_UID_FILE=… GITHUB_APP_ID=… GITHUB_APP_PRIVATE_KEY_FILE=… \
  ./moesekai-sync once --region cn --repo https://github.com/StarMoe-org/moesekai-masterdata-cn
```

| 环境变量 | 说明 |
| --- | --- |
| `MOESEKAI_CN_UID`（或 `_FILE`） | cn login 用的已存在账号 id |
| `MOESEKAI_JP_ACCOUNT_FILE` | jp 账号文件（JSON，0600）；不存在时配合 `--register` 注册一个 |
| `GITHUB_APP_ID`、`GITHUB_APP_PRIVATE_KEY`（或 `_FILE`） | 推送到 GitHub 时用；token 只申请目标仓库一个 |
