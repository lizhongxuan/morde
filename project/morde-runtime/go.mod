module morde-runtime

go 1.25.0

toolchain go1.27.1

require (
	golang.org/x/sys v0.13.0
	morde-sync v0.0.0
)

require github.com/fsnotify/fsnotify v1.9.0 // indirect

replace morde-sync => ../morde-sync
