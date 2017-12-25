package goxz

import (
	"flag"
	"go/build"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type cli struct {
	outStream, errStream io.Writer
}

const (
	exitCodeOK = iota
	exitFlagErr
	exitCodeErr
)

// Run the goxz
func Run(args []string) int {
	err := (&cli{outStream: os.Stdout, errStream: os.Stderr}).run(args)
	if err != nil {
		if err != flag.ErrHelp {
			log.Println(err)
		}
		return exitCodeErr
	}
	return exitCodeOK
}

type goxz struct {
	os, arch                              string
	name, version                         string
	dest, output, buildLdFlags, buildTags string
	zipAlways                             bool
	work                                  bool
	pkgs                                  []string

	absPkgs   []string
	platforms []*platform
	projDir   string
	workDir   string
	resources []string
}

func (cl *cli) run(args []string) error {
	log.SetOutput(cl.errStream)
	log.SetPrefix("[goxz] ")
	log.SetFlags(0)

	gx, err := cl.parseArgs(args)
	if err != nil {
		return err
	}
	if gx.projDir != "" {
		prev, err := filepath.Abs(".")
		if err != nil {
			return err
		}
		err = os.Chdir(gx.projDir)
		if err != nil {
			return err
		}
		defer os.Chdir(prev)
	}
	err = gx.init()
	if err != nil {
		return err
	}
	err = gx.prepareWorkdir()
	if err != nil {
		return err
	}
	defer func() {
		if !gx.work {
			os.RemoveAll(gx.workDir)
		}
	}()
	if gx.work {
		log.Printf("working dir: %s\n", gx.workDir)
	}

	for _, bdr := range gx.builders() {
		// XXX use goroutine and sync.ErrorGroup
		_, _ = bdr.build()
	}

	return nil
}

func (cl *cli) parseArgs(args []string) (*goxz, error) {
	gx := &goxz{}
	fs := flag.NewFlagSet("goxz", flag.ContinueOnError)
	fs.SetOutput(cl.errStream)

	fs.StringVar(&gx.name, "n", "", "Application name. By default this is the directory name.")
	fs.StringVar(&gx.dest, "d", "goxz", "Destination directory")
	fs.StringVar(&gx.version, "pv", "", "Package version")
	fs.StringVar(&gx.output, "o", "", "output")
	fs.StringVar(&gx.os, "os", "", "Specify OS (default is 'linux darwin windows')")
	fs.StringVar(&gx.arch, "arch", "", "Specify Arch (default is 'amd64')")
	fs.StringVar(&gx.buildLdFlags, "build-ldflags", "", "arguments to pass on each go tool link invocation")
	fs.StringVar(&gx.buildTags, "build-tags", "", "a space-separated list of build `tags`")
	fs.BoolVar(&gx.zipAlways, "zip", false, "zip always")

	fs.StringVar(&gx.projDir, "C", "", "[for debug] change directory")
	fs.BoolVar(&gx.work, "work", false, "[for debug] print the name of the temporary work directory and do not delete it when exiting.")

	err := fs.Parse(args)
	if err != nil {
		return nil, err
	}
	gx.pkgs = fs.Args()
	if len(gx.pkgs) == 0 {
		gx.pkgs = append(gx.pkgs, ".")
	}
	return gx, nil
}

func (gx *goxz) init() error {
	if gx.projDir == "" {
		var err error
		gx.projDir, err = filepath.Abs(".")
		if err != nil {
			return err
		}
	}

	if gx.name == "" {
		gx.name = filepath.Base(gx.projDir)
	}

	err := setupDest(gx.getDest())
	if err != nil {
		return err
	}

	// TODO: implement build constraints
	// fill the defaults
	if gx.os == "" {
		gx.os = "linux darwin windows"
	}
	if gx.arch == "" {
		gx.arch = "amd64"
	}
	gx.platforms, err = resolvePlatforms(gx.os, gx.arch)
	if err != nil {
		return err
	}
	gx.resources, err = gatherResources(gx.projDir)
	if err != nil {
		return err
	}

	gx.absPkgs, err = goAbsPkgs(gx.pkgs, gx.projDir)
	return err
}

var separateReg = regexp.MustCompile(`\s*(?:\s+|,)\s*`)

func resolvePlatforms(os, arch string) ([]*platform, error) {
	platforms := []*platform{}
	osTargets := separateReg.Split(os, -1)
	archTargets := separateReg.Split(arch, -1)
	for _, os := range osTargets {
		if strings.TrimSpace(os) == "" {
			continue
		}
		for _, arch := range archTargets {
			if strings.TrimSpace(arch) == "" {
				continue
			}
			platforms = append(platforms, &platform{os: os, arch: arch})
		}
	}
	uniqPlatforms := []*platform{}
	seen := make(map[string]struct{})
	for _, pf := range platforms {
		key := pf.os + ":" + pf.arch
		_, ok := seen[key]
		if !ok {
			seen[key] = struct{}{}
			uniqPlatforms = append(uniqPlatforms, pf)
		}
	}
	return uniqPlatforms, nil
}

func (gx *goxz) getDest() string {
	if gx.dest == "" {
		gx.dest = "goxz"
	}
	return gx.dest
}

func setupDest(dir string) error {
	err := os.Mkdir(dir, 0777)
	if err == nil || !os.IsExist(err) {
		return err
	}
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !f.Mode().IsRegular() {
			continue
		}
		n := f.Name()
		if strings.HasPrefix(n, ".zip") || strings.HasPrefix(n, ".tar.gz") {
			fpath := filepath.Join(dir, n)
			log.Printf("removing %q", fpath)
			err := os.Remove(fpath)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func goAbsPkgs(pkgs []string, projDir string) ([]string, error) {
	var gosrcs []string
	for _, gopath := range filepath.SplitList(build.Default.GOPATH) {
		gosrcs = append(gosrcs, filepath.Join(filepath.Clean(gopath), "src"))
	}
	stuff := make([]string, len(pkgs))
	for i, pkg := range pkgs {
		if strings.HasPrefix(pkg, ".") {
			absPath := filepath.Clean(filepath.Join(projDir, pkg))
			for _, gosrc := range gosrcs {
				if strings.HasPrefix(absPath, gosrc) {
					p, err := filepath.Rel(gosrc, absPath)
					if err != nil {
						return nil, err
					}
					pkg = p
					break
				}
			}
		}
		stuff[i] = pkg
	}
	return stuff, nil
}

var resourceReg = regexp.MustCompile(`(?i)^(?:readme|license|credit|install)`)

func gatherResources(dir string) ([]string, error) {
	var ret []string
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if !f.Mode().IsRegular() {
			continue
		}
		n := f.Name()
		if resourceReg.MatchString(n) && !strings.HasSuffix(n, ".go") {
			ret = append(ret, filepath.Join(dir, n))
		}
	}
	return ret, nil
}

func (gx *goxz) builders() []*builder {
	builders := make([]*builder, len(gx.platforms))
	for i, pf := range gx.platforms {
		builders[i] = &builder{
			platform:     pf,
			name:         gx.name,
			version:      gx.version,
			output:       gx.output,
			buildLdFlags: gx.buildLdFlags,
			buildTags:    gx.buildTags,
			pkgs:         gx.absPkgs,
			zipAlways:    gx.zipAlways,
			workDirBase:  gx.workDir,
			resources:    gx.resources,
		}
	}
	return builders
}

func (gx *goxz) prepareWorkdir() error {
	tmpd, err := ioutil.TempDir(gx.projDir, ".goxz-")
	gx.workDir = tmpd
	return err
}
