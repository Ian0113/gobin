/*
 The gobin command installs/runs main packages.
*/
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rogpeppe/go-internal/module"
)

const (
	debug = false
)

var (
	exitCode = 0

	fMainMod  = flag.Bool("m", false, "resolve dependencies via the main module (as given by go env GOMOD)")
	fMod      = flag.String("mod", "", "provide additional control over updating and use of go.mod")
	fRun      = flag.Bool("run", false, "run the provided main package")
	fPrint    = flag.Bool("p", false, "print gobin install cache location for main packages")
	fVersion  = flag.Bool("v", false, "print the module path and version for main packages")
	fDownload = flag.Bool("d", false, "stop after installing main packages to the gobin install cache")
	fUpgrade  = flag.Bool("u", false, "check for the latest tagged version of main packages")
	fNoNet    = flag.Bool("nonet", false, "prevent network access")
	fDebug    = flag.Bool("debug", false, "print debug information")
)

func main() {
	os.Exit(main1())
}

// TODO
//
// 1. Work out whether we want to support ... patterns
// 2. Make local step concurrent?

func main1() int {
	if err := mainerr(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func mainerr() error {
	flag.Usage = func() {
		mainUsage(os.Stderr)
	}
	flag.Parse()

	// check exclusivity of certain flags
	{
		comm := 0
		if *fRun {
			comm += 1
		}
		if *fPrint {
			comm += 1
		}
		if *fDownload {
			comm += 1
		}
		if *fVersion {
			comm += 1
		}
		if comm > 1 {
			return fmt.Errorf("the -run, -p, -v and -d flags are mutually exclusive")
		}
	}

	if *fMod != "" {
		switch *fMod {
		case "readonly", "vendor":
		default:
			return fmt.Errorf("-mod has invalid value %q", *fMod)
		}
		*fMainMod = true
	}

	if *fUpgrade && *fNoNet {
		return fmt.Errorf("the -n and -g flags are mutually exclusive")
	}

	var gopath string     // effective GOPATH
	var modCache string   // module cache path
	var modDlCache string // module download cache
	var gobinCache string // does what it says on the tin

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %v", err)
	}

	// cache path discovery
	{
		gopath = os.Getenv("GOPATH")
		if gopath != "" {
			gopath = filepath.SplitList(gopath)[0]
		} else {
			uhd := userHomeDir()
			if uhd == "" {
				return fmt.Errorf("failed to determine user home directory")
			}
			gopath = filepath.Join(uhd, "go")
		}

		// TODO I don't think the module cache path is advertised anywhere public...
		// intentionally but in case it is, replace what follows
		modCache = filepath.Join(gopath, "pkg", "mod")
		modDlCache = filepath.Join(modCache, "cache", "download")

		if *fMainMod {
			md := cwd
			for {
				if _, err := os.Stat(filepath.Join(md, "go.mod")); err == nil {
					break
				}
				if d := filepath.Dir(md); d != md {
					md = d
				} else {
					return fmt.Errorf("could not find main module")
				}
			}

			gobinCache = filepath.Join(md, ".gobincache")

		} else {
			ucd, err := os.UserCacheDir()
			if err != nil {
				return fmt.Errorf("failed to determine user cache dir: %v", err)
			}

			gobinCache = filepath.Join(ucd, "gobin")
		}
	}

	var allPkgs []*arg   // all of the non-run command line provided packages
	var runArgs []string // -r command line run args
	var netPkgs []*arg   // packages that need network resolution
	var nonMain []*arg   // non-main packages

	// prepare allPkgs
	{
		pkgPatts := flag.Args()
		if len(pkgPatts) == 0 {
			return fmt.Errorf("need to provide at least one main package")
		}
		if *fRun && len(pkgPatts) > 1 {
			pkgPatts, runArgs = pkgPatts[:1], pkgPatts[1:]
		}

		var tmpDirs []string
		defer func() {
			for _, td := range tmpDirs {
				os.RemoveAll(td)
			}
		}()

		for _, patt := range pkgPatts {
			parts := strings.SplitN(patt, "@", 2)

			a := &arg{
				patt:    patt,
				pkgPatt: parts[0],
			}

			if len(parts) == 2 {
				a.verPatt = parts[1]
			}

			if *fMainMod {
				a.wd = cwd
			} else {
				td, err := ioutil.TempDir("", "gobin")
				if err != nil {
					return fmt.Errorf("failed to create temp dir: %v", err)
				}
				tmpDirs = append(tmpDirs, td)
				if err := ioutil.WriteFile(filepath.Join(td, "go.mod"), []byte("module gobin\n"), 0644); err != nil {
					return fmt.Errorf("failed to initialise temp Go module: %v", err)
				}
				a.wd = td
			}

			allPkgs = append(allPkgs, a)
		}
	}

	if !*fUpgrade {
		// local resolution step
		for _, pkg := range allPkgs {
			proxy := "GOPROXY=file://" + modDlCache

			useModCurr := *fMainMod && pkg.verPatt == ""

			if !useModCurr {
				if err := pkg.get(proxy); err != nil {
					if *fNoNet {
						return err
					}

					netPkgs = append(netPkgs, pkg)
					continue
				}
			}

			// This is the point at which fMod == readonly
			// will fail. At the moment we return a rather gross
			// error... probably can't assume that any error
			// here is as a result of readonly... but we can
			// likely improve the error message (somehow).
			if err := pkg.list(proxy); err != nil {
				if !useModCurr {
					return err
				}

				netPkgs = append(netPkgs, pkg)
				continue
			}

			if pkg.resErr != nil {
				nonMain = append(nonMain, pkg)
			}
		}
	} else {
		netPkgs = allPkgs
	}

	if *fNoNet && len(netPkgs) > 0 {
		panic("invariant on netPkgs failed")
	}

	// network resolution step
	for _, pkg := range netPkgs {
		proxy := os.Getenv("GOPROXY")

		useModCurr := *fMainMod && pkg.verPatt == ""

		if !useModCurr {
			if err := pkg.get(proxy); err != nil {
				return err
			}
		}

		if err := pkg.list(proxy); err != nil {
			return err
		}

		if pkg.resErr != nil {
			nonMain = append(nonMain, pkg)
		}
	}

	if len(nonMain) > 0 {
		for _, pkg := range nonMain {
			fmt.Fprintf(os.Stderr, "%v@%v: %v\n", pkg.pkgPatt, pkg.verPatt, pkg.resErr)
		}
		s := ""
		if len(nonMain) > 1 {
			s = "s"
		}
		return fmt.Errorf("failed to resolve module-based main package%v", s)
	}

	for _, pkg := range allPkgs {
		// each mainPkg install must be done as a separate go command invocation because
		// we set a different GOBIN for each one.
		for _, mp := range pkg.mainPkgs {
			// calculate the relative install directory from main package import path
			// and the containing module's version
			var mainrel string
			{
				emp, err := module.EncodePath(filepath.FromSlash(mp.Module.Path))
				if err != nil {
					return fmt.Errorf("failed to encode module path %v: %v", mp.Module.Path, err)
				}

				md := emp
				if mp.Module.Version != "" {
					md = filepath.Join(md, "@v", mp.Module.Version)
				}

				epp, err := module.EncodePath(filepath.FromSlash(mp.ImportPath))
				if err != nil {
					return fmt.Errorf("failed to encode package relative path %v: %v", mp.ImportPath, err)
				}
				mainrel = filepath.Join(md, epp)
			}

			gobin := filepath.Join(gobinCache, mainrel)
			target := filepath.Join(gobin, path.Base(mp.ImportPath))

			// optimistically remove our target in case we are installing over self
			// TODO work out what to do for Windows
			if mp.ImportPath == "github.com/myitcv/gobin" {
				_ = os.Remove(target)
			}

			proxy := "file://" + modDlCache

			var stdout bytes.Buffer

			installCmd := exec.Command("go", "install", mp.ImportPath)
			installCmd.Dir = pkg.wd
			installCmd.Env = append(buildEnv("GOPROXY="+proxy), "GOBIN="+gobin)
			installCmd.Stdout = &stdout

			if err := run(installCmd); err != nil {
				return err
			}

			switch {
			case *fDownload:
				// noop
			case *fPrint:
				fmt.Println(target)
			case *fVersion:
				fmt.Printf("%v %v\n", mp.Module.Path, mp.Module.Version)
			case *fRun:
				argv := append([]string{target}, runArgs...)
				if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
					return fmt.Errorf("failed to exec %v: %v", target, err)
				}
			default:
				installBin := os.Getenv("GOBIN")
				if installBin == "" {
					installBin = filepath.Join(gopath, "bin")
				}
				if err := os.MkdirAll(installBin, 0755); err != nil {
					return fmt.Errorf("failed to mkdir %v: %v", installBin, err)
				}
				src, err := os.Open(target)
				if err != nil {
					return fmt.Errorf("failed to open %v: %v", target, err)
				}
				defer src.Close()
				bin := filepath.Join(installBin, path.Base(mp.ImportPath))

				openMode := os.O_CREATE | os.O_WRONLY

				// optimistically remove our target in case we are installing over self
				// TODO work out what to do for Windows
				if mp.ImportPath == "github.com/myitcv/gobin" {
					_ = os.Remove(bin)
					openMode = openMode | os.O_EXCL
				}

				dest, err := os.OpenFile(bin, openMode, 0755)
				if err != nil {
					return fmt.Errorf("failed to open %v for writing: %v", bin, err)
				}
				defer dest.Close()
				if _, err := io.Copy(dest, src); err != nil {
					return fmt.Errorf("failed to copy %v to %v", target, bin)
				}
				fmt.Printf("Installed %v@%v to %v\n", mp.ImportPath, mp.Module.Version, bin)
			}
		}
	}

	return nil
}

// listPkg is a convenience type for unmarshaling the output from go list
type listPkg struct {
	ImportPath string
	Name       string
	Dir        string
	Module     struct {
		Path    string
		Dir     string
		Version string
	}
}

// arg is a wrapper around a command line-provided package
type arg struct {
	patt     string     // the command line-provided pattern
	pkgPatt  string     // the package part of patt
	verPatt  string     // the version part of patt
	mainPkgs []*listPkg // main packages resolved from patt
	wd       string     // working directory for resolution
	resErr   error      // resolution error
	target   string     // the gobin cache target
}

var (
	errNonMain = errors.New("not a main package")
)

// resolve attempts to resolve a.patt to main packages, using the supplied
// proxy (if != "").  If there is an error resolving a.patt to a package and
// version this is returned. Otherwise the main packages matched by the
// packages are populated into a.mainPkgs
func (a *arg) get(proxy string) error {
	env := buildEnv(proxy)

	getCmd := exec.Command("go", "get", "-d", a.patt)
	getCmd.Dir = a.wd
	getCmd.Env = env

	if err := run(getCmd); err != nil {
		return err
	}

	return nil
}

func (a *arg) list(proxy string) error {
	env := buildEnv(proxy)

	var stdout bytes.Buffer

	listCmd := exec.Command("go", "list", "-json", a.pkgPatt)
	listCmd.Dir = a.wd
	listCmd.Stdout = &stdout
	listCmd.Env = env

	if err := run(listCmd); err != nil {
		return err
	}

	dec := json.NewDecoder(&stdout)

	// TODO if/when we support patterns including ... we will need to change the
	// semantics of a.resErr and the version resolution below

	for {
		pkg := new(listPkg)
		if err := dec.Decode(pkg); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		a.verPatt = pkg.Module.Version

		if pkg.Name != "main" {
			a.resErr = errNonMain
			return nil
		}

		a.mainPkgs = append(a.mainPkgs, pkg)
	}

	return nil
}

// buildEnv builds the correct environment for running go commands from gobin.
// proxy is expected to be empty or take the form "GOPROXY=X". If it is non
// empty it will be added to the environment.
func buildEnv(proxy string) []string {
	env := append(os.Environ(), "GO111MODULE=on")
	if proxy != "" {
		env = append(env, proxy)
	}
	goflags := os.Getenv("GOFLAGS")
	if *fMainMod && *fMod != "" {
		goflags += " -mod=" + *fMod
	}
	return append(env, "GOFLAGS="+goflags)
}

func run(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, stderr.String())
	}

	end := time.Now()

	if !debug && !*fDebug {
		return nil
	}

	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	var goenv []string
	for _, v := range env {
		if strings.HasPrefix(v, "GO") {
			goenv = append(goenv, v)
		}
	}
	fmt.Fprintf(os.Stderr, "+ cd %v; %v %v # took %v\n%s", cmd.Dir, strings.Join(goenv, " "), strings.Join(cmd.Args, " "), end.Sub(start), stderr.String())

	return nil
}
