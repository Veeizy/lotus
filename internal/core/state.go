package core

import (
	"encoding/json"
	"errors"
	"os"
)

/* ---------------- net-state.json（PROTOCOL §6.1） ---------------- */

// LoadState 读取节点状态：v1 {v,secret,name} 与 v2 {v:2,secret,name} 均接受。
// secret 必须为 16 位。文件不存在返回 existed=false。
func LoadState(path string) (secret, name string, existed bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	var st struct {
		V      int    `json:"v"`
		Secret string `json:"secret"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", "", true, err
	}
	if len(st.Secret) != 16 {
		return "", "", true, errors.New("net-state.json 密钥长度非法")
	}
	return st.Secret, st.Name, true, nil
}

// SaveState 写回 v2 格式。只读目录时调用方降级为内存态（与旧实现一致）。
func SaveState(path, secret, name string) error {
	b, err := json.MarshalIndent(struct {
		V      int    `json:"v"`
		Secret string `json:"secret"`
		Name   string `json:"name"`
	}{V: 2, Secret: secret, Name: name}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
