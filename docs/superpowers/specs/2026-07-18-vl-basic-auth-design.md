# VL Basic Auth 設計

日期：2026-07-18
狀態：已通過 brainstorming，待實作

## 目的

把 VictoriaLogs 用 HTTP Basic Auth 關起來（單一共享密鑰），還 v1「無 auth、信任網路」的技術債。寫入端（client transport）與讀取端（MCP reader）都帶憑證；三端在**未設憑證時維持現行無 auth 行為**（off-by-default，dev 與既有部署零改動）。

## 背景與決策

- VL OSS 內建 auth = `-httpAuth.username` / `-httpAuth.password`，**整台 server 一組 HTTP Basic Auth**，涵蓋所有 endpoint（`/insert` 與 `/select`）。OSS 無 per-route ACL / bearer / 讀寫 scope 分離（那是 vmauth 才有）。實測 VL v1.51.0 確認。
- **威脅模型（brainstorming 決定 A）**：受控網段「把敞開的門關上」，不區分讀寫 scope。單一共享密鑰即可，不引入 vmauth 第三個 process。
- **讀取端憑證來源（決定 A）**：獨立 env `VL_USERNAME` / `VL_PASSWORD` + 明確 `req.SetBasicAuth`。不塞進 `VL_URL` userinfo（避免隨 URL 被印進 log / 錯誤訊息外洩）。
- **TLS（決定 A）**：不納入本 task，記為 caveat。Basic Auth 在純 HTTP 上是 base64、非加密。TLS 是正交的加密問題，之後開 VL `-tls` 是獨立非破壞性步驟。
- **APIKey/Bearer（決定：移除）**：client 現送 `Authorization: Bearer <APIKey>`，但專案已全面轉 VL，VL 只吃 Basic Auth → 這行對 VL 是無效死碼。本 task 正好碰這行，一併移除 `RemoteConfig.APIKey`，改帶 Basic Auth。

## 元件

### 1. VL（ops / 文件）

以 `-httpAuth.username=<u> -httpAuth.password=<p>` 啟動即開 auth。密碼可用 `-httpAuth.password=file:///abs/path` 避免進 process args。更新 `scripts/run-dev.sh` 與 README（含開/關 auth 兩種示範）。

### 2. 寫入端 `client/`

**`client/config.go`：**
- `RemoteConfig` **移除 `APIKey`**，**新增 `Username string` / `Password string`**。
- 更新欄位 doc comment（現有註解提到「APIKey Bearer」「VL OSS 無 auth,APIKey 留空」需改寫）。

**`client/sender.go`：**
- `post()` 中把 `req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)` 換成：
  ```go
  if s.cfg.Username != "" {
      req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
  }
  ```
  Username 空 → 不帶 auth header（維持無 auth 行為）。

### 3. 讀取端 `internal/vl/`、`cmd/ducklog-mcp/`

**`internal/vl/client.go`：**
- `Client` 增 `user` / `pass` 欄位；`New` 簽章增 user/pass 參數。
- `Query` / `Count` / `Ping` 建 request 後，在 `user != ""` 時 `req.SetBasicAuth(c.user, c.pass)`。（`Count` 走 `Query`，故只需在 `Query` 與 `Ping` 兩處帶。）

**`cmd/ducklog-mcp/main.go`：**
- 讀 `VL_USERNAME` / `VL_PASSWORD` env，傳入 `vl.New`。fail-fast Ping 一併帶 auth。

### 4. 測試

- **client（免 VL、快）**：`httptest` server 斷言——設 `Username` → 請求含 `Authorization: Basic <base64(u:p)>`；未設 → 無 `Authorization` header。
- **vl reader（端到端，真 VL）**：`internal/vltest` 加 `StartVLWithAuth(t, user, pass)`（以 `-httpAuth.*` 啟動）；測「無憑證 / 錯憑證查詢 → 非 200 錯誤」「帶對憑證 → ok」。真正驗證護欄生效。

## caveat（記文件）

Basic Auth 在純 HTTP 上以 base64 傳輸、非加密；請在受信任網段使用，需端到端加密再開 VL `-tls`。

## 影響範圍

`client/{config.go,sender.go}`、`internal/vl/client.go`、`cmd/ducklog-mcp/main.go`、`internal/vltest/vltest.go` + 文件（`scripts/run-dev.sh`、README）。無新依賴。

`RemoteConfig.APIKey` 移除是 public API 破壞性變更；client module v1/低採用，已接受。
