interface DockerNetworkList {
    [networkType: string]: Docker.NetworkInfo
}

interface DockerNetworkContainerList {
    [id: string]: Docker.NetworkContainer
}