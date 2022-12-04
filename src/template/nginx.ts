import ejs from 'ejs';
import fs from 'fs';
import { ltrim } from "../"

export const nginx = async (inputFile: string, outputFile: string, containerData: ContainerInfo[]) => {
    console.log(`Processing template '${inputFile}' into '${outputFile}'`);

    // Remove all containers which don't have valid upstream configurations
    const upstreams: ContainerInfo[] = filterValidUpstreams(containerData);

    // filter the environment variables and labels so only those relating to the virtual host setup remain
    for(const container of upstreams) {
        container.env = filterEnvVars(container.env);
        container.labels = filterLabels(container.labels);
    }

    const serverList: ServerList = {};
    const upstreamList: UpstreamList = {};
    const errorPageData: {}[] = [];

    for(const container of upstreams) {
        // build a list of virtual host objects from the environment variables and labels
        const virtualHostList: VirtualHost[] = makeVirtualHostList(container);
        const processedPaths: string[] = [];

        for(const virtualHost of virtualHostList) {
            const networkLocationList: NetworkLocation[] = [];

            // Each container might be available on multiple networks
            // So we add each network to the upstream so if one is not available, it can fall back
            for(const containerNetwork of container.networks){
                networkLocationList.push({
                    name: containerNetwork.name,
                    ipAddress: containerNetwork.ipAddress,
                    port: virtualHost.port,
                });
            }

            // For every protocol, container, port combination, create an upstream that 
            // you can target using a location {} block
            const upstreamName: string = `${virtualHost.protocol}_${container.name}_${virtualHost.port}`;
            upstreamList[upstreamName] = {
                name: upstreamName,
                networks: networkLocationList,
            };

            // For every hostname (e.g: api.mycompany.com) create a server {} block
            // This will contain multiple locations that target upstreams
            if(serverList[virtualHost.host] === undefined){
                serverList[virtualHost.host] = {
                    host: virtualHost.host,
                    locations: []
                };
            }

            // For each path in every virtualhost, we need to attach to the server {} block
            // A new location that will target this location. Each location could be a regex 
            // or plain path/endpoint and it must target the upstream we created in this loop
            // which is how every server groups locations, where each location can target an upstream
            if(processedPaths.indexOf(virtualHost.path) === -1){
                serverList[virtualHost.host].locations.push({
                    path: virtualHost.path,
                    pathIsRegex: virtualHost.pathIsRegex,
                    protocol: virtualHost.protocol,
                    upstream: upstreamName,
                });
                
                processedPaths.push(virtualHost.path);
            }

            // We create a json object for each virtualhost so for the 503 error page we can
            // show some useful information for the developer to use, to make things more friendly
            errorPageData.push(Buffer.from(JSON.stringify({
                protocol: virtualHost.protocol,
                host: virtualHost.host,
                path: virtualHost.path,
                container: container.name,                
            })).toString('base64'));
        }
    }

    const data = {
        errorPageData,
        serverList: Object.values(serverList),
        upstreamList: Object.values(upstreamList),
    }

    if(process.env.DEBUG === 'verbose'){
        console.dir({data}, {depth:null});
    }

    await renderTemplate(inputFile, outputFile, data);
}

async function renderTemplate(inputFile: string, outputFile: string, data: any) {
    console.log(`Writing template '${outputFile}`);
    let template: string = await ejs.renderFile(inputFile, data, { async: true });
    template = reformatTemplate(template);
    await fs.writeFileSync(outputFile, template);
};

function makeVirtualHostFromEnvParams(envVars: EnvironmentList): VirtualHost {
    const defaultPort = 80;
    const defaultProtocol = 'http';

    const path: string = ltrim(envVars['VIRTUAL_PATH'] ?? '/', '~');

    return {
        host: envVars['VIRTUAL_HOST'],
        port: +(envVars['VIRTUAL_PORT'] ?? defaultPort),
        path,
        pathIsRegex: path.startsWith('^'),
        protocol: envVars['VIRTUAL_PROTO'] ?? defaultProtocol,
    };
}

function makeVirtualHostFromLabels(dockerProxy: string, group: string, labels: LabelList): VirtualHost {
    const defaultPort = 80;
    const defaultProtocol = 'http';

    const path: string = ltrim(labels[`${dockerProxy}.${group}.path`] ?? '/', '~');

    return {
        host: labels[`${dockerProxy}.${group}.host`],
        port: +(labels[`${dockerProxy}.${group}.port`] ?? defaultPort),
        path,
        pathIsRegex: path.startsWith('^'),
        protocol: labels[`${dockerProxy}.${group}.protocol`] ?? defaultProtocol,
    };
}

function makeVirtualHostList(container: ContainerInfo): VirtualHost[] {
    const virtualHostList: VirtualHost[] = [];

    if(container.env['VIRTUAL_HOST'] !== undefined) {
        virtualHostList.push(makeVirtualHostFromEnvParams(container.env));
    }

    const processed: string[] = [];
    for(const key of Object.keys(container.labels)){
        const [dockerProxy, group, type] = key.split('.');

        // We process labels by group, so if we found this group in the processed list
        // Then we need to skip it because they are done one group at a time
        if(processed.indexOf(group) !== -1) continue;

        virtualHostList.push(makeVirtualHostFromLabels(dockerProxy, group, container.labels));

        // Add this group to the list of those processed so it skips to the next group
        processed.push(group);
    }

    return virtualHostList;
}

function filterEnvVars(envVars: EnvironmentList): EnvironmentList {
    const filtered: EnvironmentList = {};

    for (const key in envVars) {
        if(key.startsWith('VIRTUAL')) {
            filtered[key] = envVars[key];
        }
    }
    
    return filtered;
}

function filterLabels(labels: LabelList): LabelList {
    const filtered: LabelList = {};

    for (const key in labels) {
        if(key.startsWith('docker-proxy')) {
            filtered[key] = labels[key];
        }
    }

    return filtered;
}

function filterValidUpstreams(containerData: ContainerInfo[]): ContainerInfo[] {
    return containerData.filter((container: ContainerInfo):boolean => {
        // Find a single 'host' label in order for this upstream to be potentially processible
        for(const key in container.labels){
            const [project, group, type] = key.split('.');

            // ignore all labels that don't have the correct project label
            if(project !== 'docker-proxy') continue;
            // ignore all labels apart from host, it's the only one we care about
            if(type !== 'host') continue;

            if(type === 'host' && container.labels[key].length > 0){
                return true;
            }
        }

        // Find a single VIRTUAL_HOST env var in order for this upstream to also be potentially processible
        for(const key in container.env){
            if(key === 'VIRTUAL_HOST') return true;
        }

        return false;
    });
}

function reformatTemplate(template: string): string {
    const indent = '    ';
    let count = 0;

    // This is really shit and haphazard, but it actually works quite well
    const lines:string[] = template.split("\n").reduce((doc: string[], line: string) => {
        line = line.trim();

        // do not separate a block away from the comment above it
        if (line.endsWith('{') && doc.length && !doc[doc.length - 1].startsWith('#')) {
            doc.push('');
        }

        // if this closes a block, reduce the indentation for the current line
        if (line.startsWith('}')) {
            count--;
        }

        // Add non-empty lines with appropriate indentation
        if (line.length > 0) {
            // make sure count never becomes negative
            count = Math.abs(count);
            doc.push(indent.repeat(count) + line);
        }

        // if this opens a new block, increase the indentation for the next line
        if (line.endsWith('{')) {
            count++;
        }

        // If this closes a block, and the previous line DOES NOT closes a block, add a blank line
        if (line === '}' && doc.length && !doc[doc.length - 1].endsWith('}')) {
            doc.push('');
        }

        return doc;
    }, []);

    return lines.join("\n") + "\n";
}
