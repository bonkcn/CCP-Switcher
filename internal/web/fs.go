package web

import (
	"io/fs"
)

func fsSub(path string) (fs.FS, error) {
	return fs.Sub(assets, path)
}
