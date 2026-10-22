// Package web 内嵌前端（pic 项目的 public/index.html，零改动）。
package web

import _ "embed"

//go:embed index.html
var Index []byte
