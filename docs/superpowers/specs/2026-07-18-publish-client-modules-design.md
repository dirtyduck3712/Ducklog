# 發佈 client / zapsink 模組(可 go get)設計

日期：2026-07-18
狀態：已通過設計討論，待實作

## 目的

讓外部 Go 專案能以 `go get` 安裝 ducklog 的 transport 模組，取代目前「整包複製 repo / 本機 replace 絕對路徑」的接法（無版本、會 drift、跨機器 build 失敗）。

## 問題根因

三個 module 的 path 是**裸名字**，非 VCS 可解析路徑：`module ducklog`、`module ducklog/client`、`module ducklog/zapsink`。`ducklog/client` 無 domain，Go 無法解析要去哪抓；zapsink 靠 `replace ducklog/client => ../client` 這種本機相對路徑才連得起來。外部專案只能複製或自己 replace。`docs/INTEGRATION.md` §0 已記此限制並把「改成真實網址」列為選項 B——本設計即把選項 B 變成預設並執行。

## 決策（設計討論定案）

1. **只正名要被外部 import 的兩個 lib：`client` + `zapsink`。root `module ducklog` 不動。**
   - 驗證：root 與 client/zapsink 完全解耦（0 import）；改 root 要動 23 處 internal import 卻無消費者受益 → YAGNI / 手術改變。
2. **小寫路徑基底 `github.com/dirtyduck3712/ducklog`。**
   - 驗證：`git ls-remote` 小寫 URL 已解析到同一 public repo（不 rename 也能 go get）。
   - GitHub 把 repo 從 `Ducklog` rename 成 `ducklog` 是**建議但非阻塞**的乾淨化動作（Ian 手動；不做則 module cache 會有 `!ducklog` 轉義，功能不受影響）。
3. **留 monorepo + 前綴 tag**（`client/v0.1.0`、`zapsink/v0.1.0`），不拆 repo。
4. **保留 `replace ducklog/client => ../client` 給 repo 內開發**；發佈版的 `require` 指真實 tag，consumer 走真版本不吃 replace。

## 變更

### 1. `client/go.mod`
`module ducklog/client` → `module github.com/dirtyduck3712/ducklog/client`。
（client 是單一 package，無 internal 子套件，模組內無 import path 需改。）

### 2. `zapsink/go.mod`
- `module ducklog/zapsink` → `module github.com/dirtyduck3712/ducklog/zapsink`。
- `require ducklog/client v0.0.0` → `require github.com/dirtyduck3712/ducklog/client v0.1.0`。
- `replace ducklog/client => ../client` → `replace github.com/dirtyduck3712/ducklog/client => ../client`（repo 內開發續用）。

### 3. `zapsink/*.go`
`import "ducklog/client"` → `import "github.com/dirtyduck3712/ducklog/client"`（`core.go`、`core_test.go`）。

### 4. 文件（repo 內）
- `docs/INTEGRATION.md`：§0 改寫，把 `go get github.com/dirtyduck3712/ducklog/client@latest` 設為主要接法；所有 `import "ducklog/client"` 範例改全路徑；移除/降級「本機 replace」為純本機開發備註。
- `client/README.md`、`README.md`：import path 範例改全路徑。

### 5. 文件（repo 外，另記）
- `~/.claude/skills/ducklog-integration/SKILL.md`：把「複製」改成 `go get`。**此檔在 Ian 全域 config、不在 repo**，屬獨立編輯，不進本 branch/PR。

### 6. 發佈（merge 後、與 Ian 協調的 publish 步驟）
- push `main` 後打前綴 tag：`client/v0.1.0`、`zapsink/v0.1.0`，push tags。
- 這是 external publish，push 由 Ian 授權。

## 驗證

- **repo 內即時可驗**：`cd zapsink && go build ./... && go test ./...`（吃 `replace`，證明改名後模組內部仍相容）；`cd client && go build ./... && go test ./...`。root 不動，`go build ./...` 仍綠。
- **go get 真能動（需 tag 已 push 才可驗）**：merge + push tag 後，在 `/tmp` 建一個丟棄式 consumer module：`require github.com/dirtyduck3712/ducklog/client v0.1.0` + `import` + 一個最小 `NewRemoteHandler` 呼叫，`GOFLAGS=-mod=mod go build`，證明外部 `go get` 成功。此步在發佈後做。

## 影響範圍

`client/go.mod`、`zapsink/go.mod`、`zapsink/{core.go,core_test.go}`、`docs/INTEGRATION.md`、`client/README.md`、`README.md`。root module 不動。無程式邏輯變更（純 module path / import / 文件）。

## 非目標（YAGNI）

- 不改 root `module ducklog`。
- 不拆獨立 repo。
- 不做 CI 自動 tag / release automation（首發手動打 tag）。
- 不動 client/zapsink 的任何執行邏輯。
