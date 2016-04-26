package remote

import (
	"errors"
	"fmt"

	"github.com/koding/kite"
	"github.com/koding/kite/dnode"
	"github.com/koding/logging"

	"koding/klient/remote/machine"
	"koding/klient/remote/mount"
	"koding/klient/remote/req"
	"koding/klient/remote/rsync"
	"path"
)

// CacheFolderHandler implements a prefetching / caching mechanism, currently
// implemented
func (r *Remote) CacheFolderHandler(kreq *kite.Request) (interface{}, error) {
	log := r.log.New("remote.cacheFolder")

	var params struct {
		req.Cache

		// klient uses vendored version of dnode with path rewrite that's not
		// compatible with other apps, hence we embed common fields into req.Cache
		// and specify dnode.Function by itself
		Progress dnode.Function `json:"progress"`
	}

	if kreq.Args == nil {
		return nil, errors.New("Required arguments were not passed.")
	}

	if err := kreq.Args.One().Unmarshal(&params); err != nil {
		err = fmt.Errorf(
			"remote.cacheFolder: Error '%s' while unmarshalling request '%s'\n",
			err, kreq.Args.One(),
		)
		r.log.Error(err.Error())
		return nil, err
	}

	switch {
	case params.Name == "":
		return nil, errors.New("Missing required argument `name`.")
	case params.LocalPath == "":
		return nil, errors.New("Missing required argument `localPath`.")
	case params.Username == "":
		return nil, errors.New("Missing required argument `username`.")
	case params.SSHAuthSock == "":
		return nil, errors.New("Missing required argument `sshAuthSock`.")
	}

	log = log.New(
		"mountName", params.Name,
		"localPath", params.LocalPath,
	)

	remoteMachine, err := r.GetDialedMachine(params.Name)
	if err != nil {
		log.Error("Error getting dialed, valid machine. err:%s", err)
		return nil, err
	}

	exists, err := remoteMachine.DoesRemoteDirExist(params.RemotePath)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, mount.ErrRemotePathDoesNotExist
	}

	if params.RemotePath == "" {
		// TODO: Deprecate in favor of a more robust way to identify the home dir.
		// This assumes that the username klient is running under has a home
		// at /home/username. Not true for root, if the user isn't the same user
		// as klient is running under, and not true if the homedir isn't /home.
		params.RemotePath = path.Join("/home", remoteMachine.Username)
	}

	remoteSize, err := getSizeOfRemoteFolder(remoteMachine, params.RemotePath)
	if err != nil {
		return nil, err
	}

	rs := rsync.NewClient(r.log)
	syncOpts := rsync.SyncIntervalOpts{
		SyncOpts: rsync.SyncOpts{
			Host:              remoteMachine.IP,
			Username:          params.Username,
			RemoteDir:         params.RemotePath,
			LocalDir:          params.LocalPath,
			SSHAuthSock:       params.SSHAuthSock,
			SSHPrivateKeyPath: params.SSHPrivateKeyPath,
			DirSize:           remoteSize,
		},
		Interval: params.Interval,
	}
	log.Info("Caching remote via RSync, with options:%v", syncOpts)
	progCh := rs.Sync(syncOpts.SyncOpts)

	// If a valid callback is not provided, this method blocks until the data is done
	// transferring.
	if !params.Progress.IsValid() {
		// For predictable behavior we log any errors, but do not immediately return on
		// them. If we return early, RSync may still be running - by blocking until the
		// channel is closed, we ensure that this method, in blocking form, only returns
		// after RSync is done.
		var err error
		for p := range progCh {
			if p.Error.Message != "" {
				log.Error(
					"Error encountered in blocking remote.cache. progress:%d, err:%s",
					p.Progress, p.Error.Message,
				)
				err = errors.New(p.Error.Message)
			}
		}

		// After the progress chan is done, start our SyncInterval
		startIntervaler(r.log, remoteMachine, rs, syncOpts)

		return nil, err
	}

	go func() {
		for p := range progCh {
			if p.Error.Message != "" {
				log.Error(
					"Error encountered in nonblocking remote.cache. progress:%d, err:%s",
					p.Progress, p.Error.Message,
				)
			}
			params.Progress.Call(p)
		}

		// After the progress chan is done, start our SyncInterval
		startIntervaler(log, remoteMachine, rs, syncOpts)
	}()

	return nil, nil
}

// startIntervaler starts the given rsync interval, logs any errors, and adds the
// resulting Intervaler to the Mount struct for later Stoppage.
func startIntervaler(log logging.Logger, remoteMachine *machine.Machine, c *rsync.Client, opts rsync.SyncIntervalOpts) {
	log = log.New("startIntervaler")

	if opts.Interval <= 0 {
		log.Warning(
			"startIntervaler() called with interval:%d. Cannot start Intervaler",
			opts.Interval,
		)
		return
	}

	log.Info("Creating and starting RSync SyncInterval")
	intervaler, err := c.SyncInterval(opts)
	if err != nil {
		log.Error("rsync SyncInterval returned an error:%s", err)
		return
	}

	remoteMachine.Intervaler = intervaler
}
