interface NetworkList {
    [id: string]: string,
}

interface Configuration {
    id: string,
    name: string,
    request: string,
    response: string,
    renderer: string,
    networks: NetworkList,
}

interface EnvironmentList {
    [id: string]: string
}

interface LabelList {
    [id: string]: string
}

type LabelMap = Map<string, string>;

type NetworkInfoMap = Map<string, NetworkInfo>;

interface NetworkInfo {
    name: string,
    ipAddress: string,
    id: string,
}

interface Port {
    containerPort: string,
    containerProto: string,
    hostIp?: string,
    hostPort?: string
}

type ContainerIdList = Set<string>;

interface ContainerInfo {
    id: string
    name: string,
    env: EnvironmentList,
    labels: LabelList,
    networks: NetworkInfo[],
    ports: Port[],
} 