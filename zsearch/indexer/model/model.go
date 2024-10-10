package model

type FileInfo struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Content  string `json:"content"`
}
