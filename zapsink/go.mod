module docklog/zapsink

go 1.24.0

require (
	docklog/client v0.0.0
	go.uber.org/zap v1.27.1
)

require go.uber.org/multierr v1.10.0 // indirect

replace docklog/client => ../client
