// Package web 内嵌控制台静态资源，让服务端无需额外部署前端文件即可提供界面。
package web

import _ "embed"

// ConsoleHTML 是中央控制台的单页界面。
// 用 embed 内嵌而非运行时读文件，避免部署时遗漏静态资源。
//
//go:embed console.html
var ConsoleHTML string
