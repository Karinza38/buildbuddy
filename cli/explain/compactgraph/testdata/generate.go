package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/otiai10/copy"
	"golang.org/x/tools/txtar"
)

const JavaProject = `
-- MODULE.bazel --
-- src/main/java/com/example/lib/BUILD.bazel --
java_library(
    name = "lib",
    srcs = ["Lib.java"],
    visibility = ["//visibility:public"],
)
-- src/main/java/com/example/lib/Lib.java --
package com.example.lib;

public class Lib {
    public static String getName() {
      return "Lib";
    }
}
-- src/test/java/com/example/lib/BUILD.bazel --
java_test(
    name = "lib_test",
    srcs = ["LibTest.java"],
    test_class = "com.example.lib.LibTest",
    deps = ["//src/main/java/com/example/lib"],
)
-- src/test/java/com/example/lib/LibTest.java --
package com.example.lib;

import static org.junit.Assert.assertTrue;
import org.junit.Test;

public class LibTest {
    @Test
    public void testGetName() {
        assertTrue(Lib.getName().startsWith("Lib"));
    }
}
-- src/main/java/com/example/app/BUILD.bazel --
java_library(
    name = "app_lib",
    srcs = ["App.java"],
    deps = ["//src/main/java/com/example/lib"],
)

java_binary(
    name = "app",
    runtime_deps = [":app_lib"],
)
-- src/main/java/com/example/app/App.java --
package com.example.app;

public class App {
    public static void main(String[] args) {
        System.out.println("Hello, " + com.example.lib.Lib.getName() + "!");
    }
}
`

func main() {
	// bazelisk prepends the current Bazel binary to the PATH, but we want "bazel" to reference bazelisk itself.
	originalPath := os.Getenv("PATH")[strings.IndexByte(os.Getenv("PATH"), ':')+1:]
	err := os.Setenv("PATH", originalPath)
	if err != nil {
		log.Fatalf("Failed to set PATH: %s", err)
	}

	buildWorkspaceDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if buildWorkspaceDirectory == "" {
		log.Fatalf("BUILD_WORKSPACE_DIRECTORY environment variable must be set, run with `bazel run`")
	}
	outDir := filepath.Join(buildWorkspaceDirectory, filepath.FromSlash("cli/explain/compactgraph/testdata"))
	var toGenerate map[string]bool
	if len(os.Args) > 1 {
		toGenerate = make(map[string]bool)
		for _, arg := range os.Args[1:] {
			toGenerate[arg] = true
		}
	} else {
		logs, err := filepath.Glob(filepath.Join(outDir, "*.pb.zstd"))
		if err != nil {
			log.Fatalf("Failed to glob logs: %s", err)
		}
		for _, l := range logs {
			if err := os.Remove(l); err != nil {
				log.Fatalf("Failed to remove log: %s", err)
			}
		}
	}
	for _, tc := range []struct {
		name         string
		baseline     string
		baselineArgs []string
		changes      string
		changedArgs  []string
		bazelVersion string
	}{
		{
			name:     "java_noop_impl_change",
			baseline: JavaProject,
			changes: `
-- src/main/java/com/example/lib/Lib.java --
package com.example.lib;

public class Lib {
    public static String getName() {
      return "Lib"; // no-op impl change
    }
}
`,
		},
		{
			name:     "java_impl_change",
			baseline: JavaProject,
			changes: `
-- src/main/java/com/example/lib/Lib.java --
package com.example.lib;

public class Lib {
    public static String getName() {
      return "Lib2"; // impl change
    }
}
`,
		},
		{
			name:     "java_header_change",
			baseline: JavaProject,
			changes: `
-- src/main/java/com/example/lib/Lib.java --
package com.example.lib;

public class Lib {
    public static String getName() {
      return "Lib";
    }

    public static void foo() {}
}
`,
		},
		{
			name: "env_change",
			baseline: `
-- MODULE.bazel --
-- pkg/BUILD --
genrule(
    name = "gen",
	outs = ["out"],
	cmd = "env > $@",
)
`,
			baselineArgs: []string{"--action_env=EXTRA=foo", "--action_env=OLD_ONLY=old_only", "--action_env=OLD_AND_NEW=old"},
			changedArgs:  []string{"--action_env=NEW_ONLY=new_only", "--action_env=OLD_AND_NEW=new", "--action_env=EXTRA=foo"},
		},
		{
			name: "non_hermetic",
			baseline: `
-- MODULE.bazel --
-- pkg/BUILD --
genrule(
    name = "gen",
	outs = ["out"],
	cmd = "uuidgen > $@",
)
`,
		},
		{
			name: "symlinks",
			baseline: `
-- MODULE.bazel --
bazel_dep(name = "bazel_skylib", version = "1.6.1")
-- pkg/BUILD --
load("@bazel_skylib//rules:copy_file.bzl", "copy_file")
copy_file(
    name = "first_symlink",
	src = "file",
	out = "symlink",
    allow_symlink = True,
)
copy_file(
	name = "second_symlink",
	src = "symlink",
	out = "symlink2",
	allow_symlink = True,
)
copy_file(
	name = "copy",
	src = "symlink2",
	out = "out",
)
-- pkg/file --
foo
`,
			changes: `
-- pkg/file --
not_foo
`,
			// TODO: Update to 7.4.0 when it's released.
			bazelVersion: "7661774e7c02942253691f28720db7b9c8454d2e",
		},
	} {
		if toGenerate != nil && !toGenerate[tc.name] {
			continue
		}

		tmpDir, err := os.MkdirTemp("", "explain-test-*")
		if err != nil {
			log.Fatalf("Failed to create temp dir: %s", err)
		}
		defer os.RemoveAll(tmpDir)

		extractTxtar(tmpDir, tc.baseline)
		collectLog(tc.baselineArgs, tmpDir, filepath.Join(outDir, tc.name+"_old.pb.zstd"), tc.bazelVersion)

		extractTxtar(tmpDir, tc.changes)
		collectLog(tc.changedArgs, tmpDir, filepath.Join(outDir, tc.name+"_new.pb.zstd"), tc.bazelVersion)
	}
}

func collectLog(args []string, projectDir, logPath, bazelVersion string) {
	outputBase, err := os.MkdirTemp("", "explain-testdata-*")
	if err != nil {
		log.Fatalf("Failed to create temp output base: %s", err)
	}
	defer os.RemoveAll(outputBase)
	// Bazel's output base can contain files with no write permissions.
	defer filepath.WalkDir(outputBase, func(path string, d fs.DirEntry, err error) error {
		return os.Chmod(path, 0755)
	})
	cmd := exec.Command(
		"bazel",
		"--nohome_rc", "--nosystem_rc",
		"--output_base="+outputBase,
		"test", "//...",
	)
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(
		cmd.Args,
		"--java_runtime_version=remotejdk_21",
		"--experimental_execution_log_compact_file="+logPath,
	)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if bazelVersion == "" {
		bazelVersion = "7.3.1"
	}
	cmd.Env = append(os.Environ(), "USE_BAZEL_VERSION="+bazelVersion)
	if err = cmd.Run(); err != nil {
		// Allow failures due to no tests as we always run with `bazel test`.
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 4 {
			log.Fatalf("Failed to run command: %s", err)
		}
	}
}

func extractTxtar(dir string, tar string) {
	txtarFS, err := txtar.FS(txtar.Parse([]byte(tar)))
	if err != nil {
		log.Fatalf("Failed to create txtar fs: %s", err)
	}
	err = copy.Copy(".", dir, copy.Options{
		FS:                txtarFS,
		PermissionControl: copy.AddPermission(0755)},
	)
	if err != nil {
		log.Fatalf("Failed to copy txtar fs: %s", err)
	}
}
