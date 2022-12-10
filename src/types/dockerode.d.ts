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

interface DockerContainerNetworkMap {
    [type: string]: {
        IPAMConfig?: any;
        Links?: any;
        Aliases?: any;
        NetworkID: string;
        EndpointID: string;
        Gateway: string;
        IPAddress: string;
        IPPrefixLen: number;
        IPv6Gateway: string;
        GlobalIPv6Address: string;
        GlobalIPv6PrefixLen: number;
        MacAddress: string;
    };
}