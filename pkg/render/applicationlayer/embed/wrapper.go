package embed

import (
	"fmt"
	"io/fs"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fsWrapper struct{ fs.FS }

func NewWrappedFS(f fs.FS) *fsWrapper {
	return &fsWrapper{f}
}

func (f *fsWrapper) AsMap() (map[string]string, error) {
	res := make(map[string]string)
	var walkFn fs.WalkDirFunc = func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return err
		}

		if b, err := fs.ReadFile(f, path); err != nil {
			return err
		} else {
			res[d.Name()] = string(b)
		}
		return nil
	}

	if err := fs.WalkDir(f, ".", walkFn); err != nil {
		return nil, fmt.Errorf("failed to walk core ruleset files (%w)", err)
	}

	return res, nil

}

func (f *fsWrapper) AsConfigMap(name, namespace string) (*corev1.ConfigMap, error) {
	data, err := f.AsMap()
	if err != nil {
		return nil, err
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}, nil
}
