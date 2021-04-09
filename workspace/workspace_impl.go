package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/mazrean/separated-webshell/domain"
)

var (
	imageRef   string = os.Getenv("IMAGE_URL")
	imageUser  string = os.Getenv("IMAGE_USER")
	imageCmd   string = os.Getenv("IMAGE_CMD")
	createOpts        = types.ExecConfig{
		User:         imageUser,
		WorkingDir:   fmt.Sprintf("/home/%s", imageUser),
		Cmd:          []string{imageCmd},
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}
	attachOpts = types.ExecStartCheck{
		Tty: true,
	}
	containerMap = sync.Map{}
	stopTimeout  = 10 * time.Second
)

type containerInfo struct {
	id         string
	manageChan chan struct{}
}

func containerName(userName domain.UserName) string {
	return fmt.Sprintf("user-%s", userName)
}

type Workspace struct {
	cli *client.Client
}

func NewWorkspace() (*Workspace, error) {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	ctx := context.Background()

	reader, err := cli.ImagePull(ctx, imageRef, types.ImagePullOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to pull image: %w", err)
	}
	io.Copy(os.Stdout, reader)

	return &Workspace{
		cli: cli,
	}, nil
}

func (w *Workspace) Create(ctx context.Context, userName domain.UserName) error {
	ctnName := containerName(userName)
	res, err := w.cli.ContainerCreate(ctx, &container.Config{
		Image:        imageRef,
		User:         imageUser,
		Tty:          true,
		OpenStdin:    true,
		AttachStderr: true,
		AttachStdin:  true,
		AttachStdout: true,
		StdinOnce:    true,
		Volumes:      make(map[string]struct{}),
	}, nil, nil, nil, ctnName)
	if errdefs.IsConflict(err) {
		ctnInfo, err := w.cli.ContainerInspect(ctx, ctnName)
		if err != nil {
			return fmt.Errorf("failed to inspect container: %w", err)
		}

		containerMap.Store(userName, &containerInfo{
			id:         ctnInfo.ID,
			manageChan: make(chan struct{}, 20),
		})

		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	containerMap.Store(userName, &containerInfo{
		id:         res.ID,
		manageChan: make(chan struct{}, 20),
	})

	return nil
}

func (w *Workspace) Connect(ctx context.Context, userName domain.UserName, isTty bool, winCh <-chan *domain.Window, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	iContainerInfo, ok := containerMap.Load(userName)
	if !ok {
		return errors.New("load container info error")
	}
	ctnInfo, ok := iContainerInfo.(*containerInfo)
	if !ok {
		return errors.New("parse container info error")
	}

	if len(ctnInfo.manageChan) >= 20 {
		return errors.New("too many shell")
	}

	err := w.cli.ContainerStart(ctx, ctnInfo.id, types.ContainerStartOptions{})
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("failed to start container: %w", err)
	}
	ctnInfo.manageChan <- struct{}{}
	defer func(ctnInfo *containerInfo) {
		<-ctnInfo.manageChan
		if len(ctnInfo.manageChan) == 0 {
			ctx := context.Background()
			err := w.cli.ContainerStop(ctx, ctnInfo.id, &stopTimeout)
			if err != nil {
				log.Fatalf("failed to stop container:%+v", err)
			}
		}
	}(ctnInfo)

	idRes, err := w.cli.ContainerExecCreate(ctx, ctnInfo.id, createOpts)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	if isTty {
		go func() {
			for win := range winCh {
				err := w.cli.ContainerExecResize(ctx, idRes.ID, types.ResizeOptions{
					Height: win.Height,
					Width:  win.Width,
				})
				if err != nil {
					log.Println(err)
					break
				}
			}
		}()
	}

	stream, err := w.cli.ContainerExecAttach(ctx, idRes.ID, attachOpts)
	if err != nil {
		return fmt.Errorf("failed to attach container: %w", err)
	}
	defer stream.Close()

	outputErr := make(chan error)

	go func() {
		var err error
		if isTty {
			_, err = io.Copy(stdout, stream.Reader)
		} else {
			_, err = stdcopy.StdCopy(stdout, stderr, stream.Reader)
		}
		outputErr <- err
	}()

	go func() {
		defer stream.CloseWrite()
		io.Copy(stream.Conn, stdin)
	}()

	err = <-outputErr
	if err != nil {
		return fmt.Errorf("failed to stdout: %w", err)
	}

	return nil
}

func (*Workspace) Remove(ctx context.Context, userName domain.UserName) error {
	return nil
}
