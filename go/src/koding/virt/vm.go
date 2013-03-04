package virt

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"labix.org/v2/mgo/bson"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"text/template"
	"time"
)

type VM struct {
	Id           bson.ObjectId `bson:"_id"`
	Name         string        `bson:"name"`
	Users        []*UserEntry  `bson:"users"`
	LdapPassword string        `bson:"ldapPassword"`
	IP           net.IP        `bson:"ip,omitempty"`
}

type UserEntry struct {
	Id   bson.ObjectId `bson:"id"`
	Sudo bool          `bson:"sudo"`
}

const UserIdOffset = 1000000
const RootIdOffset = 50000000

var templateDir string
var templates *template.Template

func LoadTemplates(dir string) {
	templateDir = dir
	var err error
	templates, err = template.ParseGlob(templateDir + "/lxc/*")
	if err != nil {
		panic(err)
	}
}

func (vm *VM) String() string {
	return "vm-" + vm.Id.Hex()
}

func (vm *VM) VEth() string {
	return fmt.Sprintf("veth-%x", []byte(vm.IP[12:16]))
}

func (vm *VM) MAC() net.HardwareAddr {
	return net.HardwareAddr([]byte{0, 0, vm.IP[12], vm.IP[13], vm.IP[14], vm.IP[15]})
}

func (vm *VM) Hostname() string {
	return vm.Name + ".koding.com"
}

func (vm *VM) RbdDevice() string {
	return "/dev/rbd/vms/" + vm.String()
}

func (vm *VM) File(path string) string {
	return fmt.Sprintf("/var/lib/lxc/%s/%s", vm, path)
}

func (vm *VM) OverlayFile(path string) string {
	return vm.File("overlay/" + path)
}

func (vm *VM) PtsDir() string {
	return vm.File("rootfs/dev/pts")
}

func (vm *VM) GetUserEntry(user *User) *UserEntry {
	for _, entry := range vm.Users {
		if entry.Id == user.ObjectId {
			return entry
		}
	}
	return nil
}

func LowerdirFile(path string) string {
	return "/var/lib/lxc/vmroot/rootfs/" + path
}

func (vm *VM) Prepare(users []User, nuke bool) {
	vm.Unprepare()

	// write LXC files
	prepareDir(vm.File(""), 0)
	vm.generateFile(vm.File("config"), "config", 0, false)
	vm.generateFile(vm.File("fstab"), "fstab", 0, false)

	// map rbd image to block device
	vm.mapRBD()

	// mount block device to overlay
	prepareDir(vm.OverlayFile("/"), RootIdOffset)
	if out, err := exec.Command("/bin/mount", vm.RbdDevice(), vm.OverlayFile("")).CombinedOutput(); err != nil {
		panic(commandError("mount rbd failed.", err, out))
	}

	// remove all except /home on nuke
	if nuke {
		entries, err := ioutil.ReadDir(vm.OverlayFile("/"))
		if err != nil {
			panic(err)
		}
		for _, entry := range entries {
			if entry.Name() != "home" {
				os.RemoveAll(vm.OverlayFile("/" + entry.Name()))
			}
		}
	}

	// prepare directories in overlay
	prepareDir(vm.OverlayFile("/"), RootIdOffset)           // for chown
	prepareDir(vm.OverlayFile("/lost+found"), RootIdOffset) // for chown
	prepareDir(vm.OverlayFile("/etc"), RootIdOffset)
	prepareDir(vm.OverlayFile("/home"), RootIdOffset)

	// create user homes
	for i, user := range users {
		if prepareDir(vm.OverlayFile("/home/"+user.Name), user.Uid) && i == 0 {
			prepareDir(vm.OverlayFile("/home/"+user.Name+"/Sites"), user.Uid)
			prepareDir(vm.OverlayFile("/home/"+user.Name+"/Sites/"+vm.Hostname()), user.Uid)
			websiteDir := "/home/" + user.Name + "/Sites/" + vm.Hostname() + "/website"
			prepareDir(vm.OverlayFile(websiteDir), user.Uid)
			files, err := ioutil.ReadDir(templateDir + "/website")
			if err != nil {
				panic(err)
			}
			for _, file := range files {
				if err := copyFile(templateDir+"/website/"+file.Name(), vm.OverlayFile(websiteDir+"/"+file.Name()), user.Uid); err != nil {
					panic(err)
				}
			}
			prepareDir(vm.OverlayFile("/var"), RootIdOffset)
			if err := os.Symlink(websiteDir, vm.OverlayFile("/var/www")); err != nil {
				panic(err)
			}
		}
	}

	// generate overlay files
	vm.generateFile(vm.OverlayFile("/etc/hostname"), "hostname", RootIdOffset, false)
	vm.generateFile(vm.OverlayFile("/etc/hosts"), "hosts", RootIdOffset, false)
	vm.generateFile(vm.OverlayFile("/etc/ldap.conf"), "ldap.conf", RootIdOffset, false)
	vm.MergePasswdFile()
	vm.MergeGroupFile()
	vm.MergeDpkgDatabase()

	// mount overlay
	prepareDir(vm.File("rootfs"), RootIdOffset)
	// if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "overlayfs", "-o", fmt.Sprintf("lowerdir=%s,upperdir=%s", LowerdirFile("/"), vm.OverlayFile("/")), "overlayfs", vm.File("rootfs")).CombinedOutput(); err != nil {
	if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "aufs", "-o", fmt.Sprintf("br=%s:%s", vm.OverlayFile("/"), LowerdirFile("/")), "aufs", vm.File("rootfs")).CombinedOutput(); err != nil {
		panic(commandError("mount overlay failed.", err, out))
	}

	// mount devpts
	prepareDir(vm.PtsDir(), RootIdOffset)
	if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "devpts", "-o", "rw,noexec,nosuid,gid="+strconv.Itoa(RootIdOffset+5)+",mode=0620", "devpts", vm.PtsDir()).CombinedOutput(); err != nil {
		panic(commandError("mount devpts failed.", err, out))
	}
	chown(vm.PtsDir(), RootIdOffset, RootIdOffset)
	chown(vm.PtsDir()+"/ptmx", RootIdOffset, RootIdOffset+5)
}

func (vm *VM) Unprepare() error {
	var firstError error

	// stop VM
	out, err := vm.Stop()
	if vm.GetState() != "STOPPED" {
		panic(commandError("Could not stop VM.", err, out))
	}

	// backup dpkg database for statistical purposes
	os.Mkdir("/var/lib/lxc/dpkg-statuses", 0755)
	copyFile(vm.OverlayFile("/var/lib/dpkg/status"), "/var/lib/lxc/dpkg-statuses/"+vm.String(), RootIdOffset)

	// unmount and unmap everything
	if out, err := exec.Command("/bin/umount", vm.PtsDir()).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount devpts failed.", err, out)
	}
	if out, err := exec.Command("/bin/umount", vm.File("rootfs")).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount overlay failed.", err, out)
	}
	if out, err := exec.Command("/bin/umount", vm.OverlayFile("")).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount rbd failed.", err, out)
	}
	if out, err := exec.Command("/usr/bin/rbd", "unmap", vm.RbdDevice()).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("rbd unmap failed.", err, out)
	}

	// remove VM directory
	os.Remove(vm.File("config"))
	os.Remove(vm.File("fstab"))
	os.Remove(vm.File("rootfs"))
	os.Remove(vm.File("rootfs.hold"))
	os.Remove(vm.OverlayFile("/"))
	os.Remove(vm.File(""))

	return firstError
}

func (vm *VM) mapRBD() {
	makeFileSystem := false
	if err := exec.Command("/usr/bin/rbd", "map", "--pool", "vms", vm.String()).Run(); err != nil {
		exitError, isExitError := err.(*exec.ExitError)
		if !isExitError || exitError.Sys().(syscall.WaitStatus).ExitStatus() != 1 {
			panic(err)
		}

		// create disk and try to map again
		if out, err := exec.Command("/usr/bin/rbd", "create", "--pool", "vms", "--size", "1200", vm.String()).CombinedOutput(); err != nil {
			panic(commandError("rbd create failed.", err, out))
		}
		if out, err := exec.Command("/usr/bin/rbd", "map", "--pool", "vms", vm.String()).CombinedOutput(); err != nil {
			panic(commandError("rbd map failed.", err, out))
		}

		makeFileSystem = true
	}

	// wait for rbd device to appear
	for {
		_, err := os.Stat(vm.RbdDevice())
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			panic(err)
		}
		time.Sleep(time.Second / 2)
	}

	if makeFileSystem {
		if out, err := exec.Command("/sbin/mkfs.ext4", vm.RbdDevice()).CombinedOutput(); err != nil {
			panic(commandError("mkfs.ext4 failed.", err, out))
		}
	}
}

func commandError(message string, err error, out []byte) error {
	return errors.New(message + "\n" + err.Error() + "\n" + string(out))
}

// may panic
func prepareDir(path string, id int) bool {
	created := true
	if err := os.Mkdir(path, 0755); err != nil {
		if !os.IsExist(err) {
			panic(err)
		}
		created = false
	}

	chown(path, id, id)

	return created
}

// may panic
func (vm *VM) generateFile(path, template string, id int, executable bool) {
	file, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	if err := templates.ExecuteTemplate(file, template, vm); err != nil {
		panic(err)
	}

	var mod os.FileMode = 0644
	if executable {
		mod = 0755
	}
	if err = file.Chmod(mod); err != nil {
		panic(err)
	}

	chown(path, id, id)
}

// may panic
func chown(path string, uid, gid int) {
	if err := os.Chown(path, uid, gid); err != nil {
		panic(err)
	}
}

func copyFile(src, dst string, id int) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	df, err := os.Create(dst)
	if err != nil {
		return err
	}

	defer sf.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}

	if err := df.Chown(id, id); err != nil {
		return err
	}
	info, err := sf.Stat()
	if err != nil {
		return err
	}
	if err := df.Chmod(info.Mode()); err != nil {
		return err
	}

	return nil
}
