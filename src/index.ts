import Docker from 'dockerode';
import DockerEE from 'docker-event-emitter';
import * as renderer from './template';
import { Duplex } from 'stream';

// If a container fails to send/receive template, we need to sleep for a period of time
// and try again, for containers that need a second or two to start up, so this prevents
// weird failures that a refresh or something can fix. This will let that happen semi-automatically
const sleep = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));

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
    stream.on('network.connect', processEvent(docker));
    stream.on('network.disconnect', processEvent(docker));

    // Start listening for events
    await stream.start();
};

main().catch(err => {
    console.log(err.message, err.stack);
    process.exit(1);
});

// Why-o-why doesn't javascript have string trimming functions built in? How ridiculous!
export function ltrim(string: string, charlist: string = "\\s"): string {
    return string.replace(new RegExp("^[" + charlist + "]+"), "");
}

export function rtrim(string: string, charlist: string = "\\s"): string {
    return string.replace(new RegExp("[" + charlist + "]+$"), "");
}

export function trim(string: string, charlist: string = "\\s"): string {
    return rtrim(ltrim(string, charlist), charlist);
}

/**
 * Docker Network event handler
 * 
 * When a new docker network event is received, it might be necessary to render
 * new templates with different configurations for the parent containers
 * to handle. So this triggers a reload/refresh of all those configurations
 * 
 * @param Docker docker
 * @returns 
 */
function processEvent(docker: Docker) {
    return async (event: any) => {
        const networkName: string = event.Actor.Attributes.name;

        // Ignore events on the bridge network
        if(networkName === 'bridge'){
            return;
        }

        // I found a 1 second delay makes it a lot more robust
        // If this code is too fast to execute, it can attempt to render configurations
        // for containers which are starting up and 1 second delay helps
        // to make sure the container is started first
        await sleep(1000);

        if(process.env.DEBUG){
            console.dir({event}, { depth: null });
        }else{
            console.log(`Network ${event.Action}: ${networkName}`);
        }

        await update(docker);
    };
}

/**
 * Update all the configurations and render all the templates
 * @param docker 
 */
async function update(docker: Docker) {
    console.log("Updating...");

    // Create a configuration array based on the container list
    const configList: ConfigGenList = makeConfigList(await docker.listContainers());

    for (const config of configList) {
        // Build a map of every container id as key to a list of networks ecah 
        const containerIdList: ContainerIdList = await makeContainerIdList(docker, config.networks);

        // Get the container information from every container found on all the networks
        const containerList: ContainerList = await makeContainerList(docker, containerIdList, config.networks);

        // Select the renderer for containers
        const templateRenderer = renderer[config.renderer];

        if(templateRenderer === undefined){
            console.log("There is no renderer enabled for this configuration");
            continue;
        }

        // Success lets us know when the template was rendered correctly and we can quit early
        let success = false;

        // controlling the number of rendering retries
        let retryCounter = 0;
        const maxRetry = 5;

        // if no success then try again but only if retryCounter is less than maxRetry
        while(success === false && retryCounter < maxRetry) {
            try{
                // Request the parent container give the template as a base64 encoded 
                // string and decode it into the text form
                const template: string = await receiveTemplate(docker, config);
    
                // Process the container list and render whatever templates it requires
                const output: string = await templateRenderer(template, containerList);
    
                // Send the template back to the parent container as a base64 encoded string
                sendTemplate(docker, config, output);

                success = true;
            }catch(error) {
                if(process.env.DEBUG){
                    console.log({error});
                }
                
                await sleep(1000);
                retryCounter++;
    
                if(retryCounter < maxRetry){
                    console.log(`Retrying '${retryCounter}' of '${maxRetry}'...`);
                }else{
                    console.log("We have retried the maximum number of times, skipping over this container!");
                }
            }
        }
    }
};

/**
 * Get a unique list of all the container id's on a given list of networks
 * 
 * @param Docker docker
 * @param networks 
 * @returns ContainerIdList
 */
async function makeContainerIdList(docker: Docker, networks: NetworkMap): Promise<ContainerIdList> {
    const idList: ContainerIdList = new Set<string>();

    for (const network of Object.values(networks)){
        const networkObject: Docker.Network = docker.getNetwork(network.id);
        const inspectData: Docker.NetworkInspectInfo = await networkObject.inspect();
        const containerList: DockerNetworkContainerList = inspectData.Containers ?? {};
        const containerIdList: string[] = Object.keys(containerList);

        containerIdList.forEach(idList.add, idList);
    }

    return idList;
}

function makeNetworkList(inputNetworks: DockerContainerNetworkMap, allowedNetworks: NetworkMap | undefined = undefined): NetworkMap
{
    const outputNetworks: NetworkMap = {};

    for (const [networkName, networkData] of Object.entries(inputNetworks)) {
        if(networkName === 'bridge') {
            if(process.env.DEBUG){
                console.log(`Skipping over network '${networkName} because we do not process bridge networks`);
            }

            continue;
        }

        // we need to filter the input networks against the allowed networks
        if(allowedNetworks !== undefined && allowedNetworks[networkData.NetworkID] === undefined){
            if(process.env.DEBUG){
                console.log(`Skipping over network '${networkName}' because not in the allowed networks`);
            }
        }else{
            outputNetworks[networkData.NetworkID] = {
                name: networkName,
                id: networkData.NetworkID,
                ipAddress: networkData.IPAddress,
            };
        }
    }

    return outputNetworks;
}

/**
 * Convert the Container Id List into an array of ContainerInfo objects which have all the extracted
 * information the renderers need to process them into templates. The networks are provided to filter
 * the configurations network list so unwanted configurations are removed
 * 
 * @param Docker docker
 * @param containerIdList 
 * @param networks 
 * @returns ContainerList
 */
async function makeContainerList(docker: Docker, containerIdList: ContainerIdList, allowedNetworks: NetworkMap): Promise<ContainerList> {
    let containerData: ContainerList = [];

    for(const containerId of containerIdList){
        const container: Docker.Container = docker.getContainer(containerId);
        const inspectData: Docker.ContainerInspectInfo = await container.inspect();

        containerData.push({
            id: inspectData.Id,
            name: trim(inspectData.Name, '/'),
            env: makeEnvList(inspectData.Config.Env),
            labels: inspectData.Config.Labels,
            networks: makeNetworkList(inspectData.NetworkSettings.Networks, allowedNetworks),
            ports: makePortList(inspectData.NetworkSettings.Ports),
        });
    }

    return containerData;
}

function getConfigGenLabels(container: Docker.ContainerInfo): ConfigGenLabels {
    const request: string | undefined = container.Labels['docker-config-gen.request'];
    const response: string = container.Labels['docker-config-gen.response'];
    const renderer: string = container.Labels['docker-config-gen.renderer'];

    if([request, response, renderer].findIndex(item => item === undefined) !== -1){
        throw new NotConfigGenContainer();
    }

    return {request, response, renderer};
}

/**
 * Process the given list of containers from the docker daemon, to filter which ones
 * contain "Parent" containers (containers which require templates to be rendered).
 * For each container, add a Configuration object which contains all the information needed
 * to process into the final templates
 * 
 * @param containerList 
 * @returns Configuration[]
 */
function makeConfigList(containerList: Docker.ContainerInfo[]): ConfigGenList {
    const configList: ConfigGenList = [];

    // Get all the configurations from each container
    for (const container of containerList) {
        try{
            // Only process containers that have the correct labels
            const labels: ConfigGenLabels = getConfigGenLabels(container);

            configList.push({
                id: container.Id,
                name: trim(container.Names[0], '/'),
                request: labels.request,
                response: labels.response,
                renderer: labels.renderer,
                // Remap the network list, removing bridge in the same process to in format: id => name
                networks: makeNetworkList(container.NetworkSettings.Networks)
            });
        }catch(error){
            // We do nothing, just skip this container and move onto the next
        }
    }; 
 
    console.log("Found configurations: ");
    if(configList.length > 0){
        console.dir(configList, {depth:null});
    }else{
        console.log("Found no configurations...");
    }

    return configList;
}

/**
 * Convert the environment variable list into a usable map
 * @param envVars 
 * @returns EnvironmentMap
 */
function makeEnvList(envVars: string[]): EnvironmentMap {
    const filtered: EnvironmentMap = {};

    for (let entry of envVars) {
        const [key, value] = entry.split('=');
        
        filtered[key] = value;
    }
    
    return filtered;
}

/**
 * Convert the docker port list into a simplified structure and remap it
 * @param ports 
 * @returns PortList
 */
function makePortList(ports: DockerPortList): PortList {
    const remap: PortList = [];

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

async function containerExec(container: Docker.Container, execCommand: string): Promise<string> {
    try{
        console.log(`Calling '${execCommand}' on container '${container.id}'`);
                
        const e: Docker.Exec = await container.exec({ 
            Cmd: execCommand.split(' '),
            AttachStdout: true,
            AttachStderr: true,
        });
        
        const s: Duplex = await e.start({});

        let template: string = "";

        const streamName = {0: 'STDIN', 1: 'STDOUT', 2: 'STDERR'};
        for await (const chunk of s) {
            const type: number = chunk.readInt8(0);
            
            if(streamName[type] === 'STDOUT'){
                template += chunk.subarray(8).toString('utf-8');
            }
        }

        return Buffer.from(template, 'base64').toString('utf-8');
    }catch(error){
        console.log(`Could not call '${execCommand}' on container '${container.id}' because docker returned an error saying '${(error as any).reason}'`);
        throw error;
    }
}

/**
 * Calls the request script on the container it got from the docker container labels
 * so it can receive the template as a base64 encoded string, then decode it and pass
 * it back so it can be used to render the final template
 * 
 * @param Docker docker 
 * @param Configuration config 
 * @returns string
 */
async function receiveTemplate(docker: Docker, config: ConfigGen): Promise<string>
{
    return await containerExec(docker.getContainer(config.id), config.request);
}

/**
 * Send the rendered template back to the container as a base64 encoded string
 * 
 * @param Docker docker 
 * @param Configuration config 
 * @param string template 
 */
async function sendTemplate(docker: Docker, config: ConfigGen, template: string)
{
    return await containerExec(docker.getContainer(config.id), config.response);
}