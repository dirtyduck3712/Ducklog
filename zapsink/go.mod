module github.com/dirtyduck3712/ducklog/zapsink

go 1.24.0

require (
	github.com/dirtyduck3712/ducklog/client v0.1.0
	go.uber.org/zap v1.27.1
)

require go.uber.org/multierr v1.10.0 // indirect

replace github.com/dirtyduck3712/ducklog/client => ../client
