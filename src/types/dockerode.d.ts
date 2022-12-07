interface DockerNetworkList {
    [networkType: string]: Docker.NetworkInfo
}

interface DockerNetworkContainerList {
    [id: string]: Docker.NetworkContainer
}

interface DockerPortList {
    [portAndProtocol: string]: Array<{
        HostIp: string;
        HostPort: string;
    }>;
}
