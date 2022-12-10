interface ConfigGenLabels {
    request: string,
    response: string,
    renderer: string,
}

interface ConfigGen {
    id: string,
    name: string,
    request: string,
    response: string,
    renderer: string,
    networks: NetworkInfoList,
}

type ConfigGenList = ConfigGen[];

interface EnvironmentMap {
    [id: string]: string
}

interface LabelMap {
    [id: string]: string
}

interface Network {
    name: string,
    ipAddress: string,
    id: string,
}

interface NetworkMap {
    [id: string]: Network;
}

interface Port {
    containerPort: string,
    containerProto: string,
    hostIp?: string,
    hostPort?: string
}

type PortList = Port[];

type ContainerIdList = Set<string>;

interface Container {
    id: string
    name: string,
    env: EnvironmentMap,
    labels: LabelMap,
    networks: NetworkMap,
    ports: PortList,
}

type ContainerList = Container[];

class NotConfigGenContainer extends Error {};