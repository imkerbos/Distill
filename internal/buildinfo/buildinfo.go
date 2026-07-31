// Package buildinfo 暴露构建期注入的版本信息。
package buildinfo

// version 由构建时 -ldflags 覆盖；本地开发保持 dev。
var version = "dev"

// Version 返回当前二进制的版本号。
// 平台会把策略下发到生产集群，事故回溯时必须能确定当时跑的是哪个版本。
func Version() string {
	return version
}
