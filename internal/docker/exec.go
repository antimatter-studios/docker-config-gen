package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ContainerExec runs a command on a container, optionally base64-encoding args
// and base64-decoding the output.
func (c *Client) ContainerExec(ctx context.Context, containerID string, execCommand string, args []string, decodeBase64 bool) (string, error) {
	// Build the final command: split the exec command and append base64-encoded args
	cmdParts := strings.Fields(execCommand)
	for _, arg := range args {
		encoded := base64.StdEncoding.EncodeToString([]byte(arg))
		cmdParts = append(cmdParts, encoded)
	}

	finalCommand := strings.Join(cmdParts, " ")
	if len(finalCommand) > 50 {
		log.Printf("Calling '%s...(truncated)' on container '%s'", finalCommand[:50], containerID)
	} else {
		log.Printf("Calling '%s' on container '%s'", finalCommand, containerID)
	}

	execConfig := container.ExecOptions{
		Cmd:          cmdParts,
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := c.cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return "", fmt.Errorf("could not call '%s' on container '%s': %w", execCommand, containerID, err)
	}

	resp, err := c.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", fmt.Errorf("could not attach to exec on container '%s': %w", containerID, err)
	}
	defer resp.Close()

	// Docker multiplexes stdout and stderr into a single stream.
	// stdcopy.StdCopy demultiplexes it for us.
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	if err != nil {
		return "", fmt.Errorf("reading exec output from container '%s': %w", containerID, err)
	}

	output := stdout.String()

	if decodeBase64 {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output))
		if err != nil {
			return "", fmt.Errorf("decoding base64 output from container '%s': %w", containerID, err)
		}
		return string(decoded), nil
	}

	return output, nil
}

// ReceiveTemplate calls the request script on a config container to get the template.
func (c *Client) ReceiveTemplate(ctx context.Context, cfg interface{ GetID() string; GetRequest() string }) (string, error) {
	return c.ContainerExec(ctx, cfg.GetID(), cfg.GetRequest(), nil, true)
}

// SendTemplate sends the rendered template back to the config container.
func (c *Client) SendTemplate(ctx context.Context, cfg interface{ GetID() string; GetResponse() string }, template string) (string, error) {
	return c.ContainerExec(ctx, cfg.GetID(), cfg.GetResponse(), []string{template}, false)
}
