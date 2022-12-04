import Docker from 'dockerode';
import DockerEE from 'docker-event-emitter';
import * as renderer from './template';

async function main() {
    console.log("Starting docker config gen");

    // Create a new Docker connection
    const docker = new Docker({
        socketPath: process.env.DOCKER_SOCKET ?? '/var/run/docker.sock',
    });

    // Update and write all the configurations upon startup
    await update(docker);

    // Setup the DockerEventEmitter
    const stream = new DockerEE(docker);
    stream.on('network.connect', connect(docker));
    stream.on('network.disconnect', disconnect(docker));

    // Start listening for events
    await stream.start();
};

main().catch(err => {
    console.log(err.message, err.stack);
    process.exit(1);
});

export function ltrim(string: string, charlist: string = "\\s"): string {
    return string.replace(new RegExp("^[" + charlist + "]+"), "");
}

export function rtrim(string: string, charlist: string = "\\s"): string {
    return string.replace(new RegExp("[" + charlist + "]+$"), "");
}

export function trim(string: string, charlist: string = "\\s"): string {
    return rtrim(ltrim(string, charlist), charlist);
}

function connect(docker: Docker) {
    return async (event: any) => {
        const networkName: string = event.Actor.Attributes.name;

        // Ignore events on the bridge network
        if(networkName === 'bridge'){
            return;
        }

        if(process.env.DEBUG == 'verbose'){
            console.dir({ NETWORK_CONNECT: event }, { depth: null });
        }else{
            console.log("Network connected: " + networkName);
        }

        await update(docker);
    };
}

function disconnect(docker: Docker) {
    return async (event: any) => {
        const networkName: string = event.Actor.Attributes.name;

        // Ignore events on the bridge network
        if(networkName === 'bridge'){
            return;
        }

        if(process.env.DEBUG == 'verbose'){
            console.dir({ NETWORK_DISCONNECT: event }, { depth: null });
        }else{
            console.log("Network disconnected: " + networkName);
        }

        await update(docker);
    };
}

async function update(docker: Docker) {
    console.log("Updating...");

    // Find all the containers to find ones which have docker-config-gen configuration labels
    const containerList = await getConfigGenContainerList(docker);
    // Create a configuration array based on the container list
    const configList: Configuration[] = createConfigList(containerList);

    // This could be configurable in the future
    const templateRenderer = renderer.nginx;

    for (const config of configList) {
        // Build a map of every container id as key to a list of networks ecah 
        const containerIdList: Set<string> = await getContainerIdList(docker, config.networks);

        // Get the container information from every container found on all the networks
        const containerList: ContainerInfo[] = await getContainerData(docker, containerIdList, config.networks);

        await templateRenderer(config.input, config.output, containerList);

        await runTrigger(docker, config);
    }
};

async function runTrigger(docker: Docker, config: Configuration)
{
    console.log(`Calling '${config.exec}' on container '${config.id}'`);
    let c = docker.getContainer(config.id);
    let e: Docker.Exec = await c.exec({ Cmd: config.exec.split(' ') });
    await e.start({});
}

async function getConfigGenContainerList(docker: Docker): Promise<Docker.ContainerInfo[]> {
    return (await docker.listContainers()).filter(container => {
        const input: string = container.Labels['docker-config-gen.input'];
        const output: string = container.Labels['docker-config-gen.output'];

        return input && output;
    });
}

function createConfigList(containerList: Docker.ContainerInfo[]): Configuration[]{
    const configList: Configuration[] = [];

    // Get all the configurations from each container
    for (const container of containerList) {
        // Remap the network list, removing bridge in the same process to in format: id => name
        const networks: NetworkList = createNetworkList(container.NetworkSettings.Networks);

        configList.push({
            id: container.Id,
            name: trim(container.Names[0], '/'),
            input: process.env.CONFIG_PATH + '/' + container.Labels['docker-config-gen.input'],
            output: process.env.CONFIG_PATH + '/' + container.Labels['docker-config-gen.output'],
            exec: trim(container.Labels['docker-config-gen.exec'] ?? 'echo no exec action defined', '"'),
            networks,
        });
    }; 
 
    console.log("Found configurations: ");
    console.dir(configList, {depth:null});

    return configList;
}

function createNetworkList(networkInfo: DockerNetworkList): NetworkList {
    const networkList: NetworkList = {};

    Object.entries(networkInfo).filter(([name, network]:[string, Docker.NetworkInfo]):boolean => {
        return name !== 'bridge';
    }).forEach(([name, network]:[string, Docker.NetworkInfo]) => {
        networkList[network.NetworkID] = name;
    });

    return networkList;
}

async function getContainerIdList(docker: Docker, networks: NetworkList): Promise<Set<string>> {
    const idList: Set<string> = new Set<string>();

    for (const key of Object.keys(networks)){
        const network: Docker.Network = docker.getNetwork(key);
        const inspectData: Docker.NetworkInspectInfo = await network.inspect();
        const containerList: DockerNetworkContainerList = inspectData.Containers ?? {};
        const containerIdList: string[] = Object.keys(containerList);

        containerIdList.forEach(idList.add, idList);
    }

    return idList;
}

async function getContainerData(docker: Docker, containerIdList: Set<string>, networks: NetworkList): Promise<ContainerInfo[]> {
    let containerData: ContainerInfo[] = [];

    for(const containerId of containerIdList){
        const container: Docker.Container = docker.getContainer(containerId);
        const inspectData: Docker.ContainerInspectInfo = await container.inspect();

        const networkInfoList: NetworkInfo[] = [];
        for (const [networkName, networkData] of Object.entries(inspectData.NetworkSettings.Networks)) {
            if(networks[networkData.NetworkID] === undefined){
                if(process.env.DEBUG == 'verbose'){
                    console.log(`Skipping over network '${networkName}' because not in the allowed networks`);
                }

                continue;
            }

            networkInfoList.push({
                name: networkName,
                id: networkData.NetworkID,
                ipAddress: networkData.IPAddress,
            })
        }

        containerData.push({
            id: inspectData.Id,
            name: trim(inspectData.Name, '/'),
            env: makeEnvList(inspectData.Config.Env),
            labels: inspectData.Config.Labels as LabelList,
            networks: networkInfoList,
            ports: makePortList(inspectData.NetworkSettings.Ports),
        });
    }

    return containerData;
}

function makeEnvList(envVars: string[]): EnvironmentList {
    const filtered: EnvironmentList = {};

    for (let entry of envVars) {
        const [key, value] = entry.split('=');
        
        filtered[key] = value;
    }
    
    return filtered;
}

function makePortList(ports): Port[] {
    const remap: Port[] = [];

    for(const [containerMap, portMap] of Object.entries(ports)){
        const containerPort: string|null = containerMap.split('/').shift() ?? null;
        const containerProto: string|null = containerMap.split('/').pop() ?? null;

        if(containerPort === null || containerProto === null) {
            console.log("ERROR: containerPort or containerProto cannot be null");
            console.dir({containerMap, portMap}, {depth:null});
            continue;
        }

        if(portMap === null) {
            remap.push({
                containerPort,
                containerProto,
            });
        }else if(portMap instanceof Array) {
            for(const hostMap of portMap){
                const hostIp: string = hostMap['HostIp'] ?? null;
                const hostPort: string = hostMap['HostPort'] ?? null;

                if(hostIp === null || hostPort === null) {
                    console.log("ERROR: If there is a mapping, it must contain HostIp and HostPort fields in order to be decoded correctly");
                    console.dir({hostMap}, {depth:null});
                    continue;
                }

                remap.push({
                    containerPort,
                    containerProto,
                    hostIp,
                    hostPort,
                }); 
            }
        }
    }

    return remap;
}