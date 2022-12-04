interface NetworkList {
    [id: string]: string,
}

interface Configuration {
    id: string,
    name: string,
    input: string,
    output: string,
    exec: string,
    networks: NetworkList,
}

interface EnvironmentList {
    [id: string]: string
}

interface LabelList {
    [id: string]: string
}

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

interface ContainerInfo {
    id: string
    name: string,
    env: EnvironmentList,
    labels: LabelList,
    networks: NetworkInfo[],
    ports: Port[],
} 