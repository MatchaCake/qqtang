package probe

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBundledONNXRuntimePlatformPaths(t *testing.T) {
	for _, test := range []struct{ goos, arch, name string }{
		{"windows", "amd64", "onnxruntime.dll"},
		{"linux", "amd64", "libonnxruntime.so.1.29.0"},
		{"linux", "arm64", "libonnxruntime.so.1.29.0"},
	} {
		got, err := bundledONNXRuntimePath(test.goos, test.arch)
		want := filepath.Join("..", "runtime", "onnxruntime", test.goos+"-"+test.arch, test.name)
		if err != nil || got != want {
			t.Fatalf("%s/%s path = %q, %v; want %q", test.goos, test.arch, got, err, want)
		}
	}
}

func TestLoadConfigSelectsONNXRuntimeLibraryOnlyWhenNeeded(t *testing.T) {
	for _, test := range []struct{ name, ai, want string }{
		{"automatic", `{"enabled":true,"backend":"onnxruntime","model_path":"models/actor.onnx"}`, "auto"},
		{"explicit", `{"enabled":true,"backend":"onnxruntime","model_path":"models/actor.onnx","shared_library_path":"custom/runtime-library"}`, "custom/runtime-library"},
		{"disabled", `{"enabled":false,"backend":"onnxruntime"}`, ""},
		{"native", `{"enabled":true,"backend":"native","model_path":"models/actor.onnx"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "server.json")
			data := `{"competitive_ai":` + test.ai + `,"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:0","response":{}}]}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			want := test.want
			if want == "auto" {
				want, err = bundledONNXRuntimePath(runtime.GOOS, runtime.GOARCH)
				if err != nil {
					t.Fatal(err)
				}
			}
			if want != "" {
				want = filepath.Join(dir, want)
			}
			if config.CompetitiveAI.SharedLibraryPath != want {
				t.Fatalf("library = %q; want %q", config.CompetitiveAI.SharedLibraryPath, want)
			}
			if test.name == "disabled" {
				config.CaptureRoot = t.TempDir()
				// Even deliberately nonexistent AI assets must not be loaded.
				config.CompetitiveAI.ModelPath = filepath.Join(dir, "missing.onnx")
				config.CompetitiveAI.SharedLibraryPath = filepath.Join(dir, "missing.dll")
				server, err := New(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := server.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
