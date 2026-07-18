# ducklog-integration skill

`SKILL.md` 是把 Go 服務接進 ducklog（→ VictoriaLogs）的 Claude Code skill。
此處為版控的 canonical 來源。

## 為何在這裡 + 如何全域啟用

這個 skill 要在**目標服務的 repo**（而非 ducklog repo）裡執行，所以必須讓
Claude Code **全域**載入它——但 project skill 只在 cwd 落於本 repo 時才載入。
因此 canonical 檔放這裡（有版控），全域位置用 symlink 指回來：

```bash
ln -s "$(pwd)/.claude/skills/ducklog-integration" ~/.claude/skills/ducklog-integration
```

（在 ducklog repo 根目錄執行。新機器 / 重新 clone 後需重建這條 symlink。）
