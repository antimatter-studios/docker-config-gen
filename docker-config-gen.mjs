import ejs from 'ejs';
import fs from 'fs';
import Docker from 'dockerode';
import DockerEE from 'docker-event-emitter';

// FIXME: There is a race condition where the docker-config-gen program can get a request
// FIXME: to generate a configuration, before the requesting container can write the template
// FIXME: meaning it'll fail because the template file does not exist yet, and will need
// FIXME: to wait some seconds before it can do that. I should build some logic which can
// FIXME: detect missing template files, watch them and generate them when the file is
// FIXME: eventually written

// FIXME: There is another problem where docker-proxy will generate nginx configurations
// FIXME: for itself, which is not valid, but I don't want to encode into this project
// FIXME: a special exception. So I should make a way for a container to say "ignore certain containers"
// FIXME: which would give the docker-proxy a way to tell docker-config-gen program to 
// FIXME: ignore docker-proxy when passing the template to be rendered
// FIXME: Or perhaps each configuration can be told it's own containerId, so when the template
// FIXME: is processing the container list, it has the option to know what "itself" is
// FIXME: and skip generating those problematic configurations

// FIXME: what if a malicious container joins the server, passing a template which just dumps the entire
// FIXME: container list and all it's environment parameters, etc to a file and exfiltrates
// FIXME: it to a remote server. That would be pretty bad. We need to prevent this from happening
// FIXME: somehow
// FIXME: Maybe this is why we can't just pass the entire container list with everything
// FIXME: we know to a rando-template-from-some-cool-container-that-asked-for-it(tm)
// FIXME: I think the solution is the concept of "renderers" where each type of file
// FIXME: the docker-config-gen project supports, has an associated renderer with it

(async function main() {
  const options = {
    configList: [],
  };

  // Create a new Docker connection
  const docker = new Docker({
    socketPath: process.env.DOCKER_SOCKET ?? '/var/run/docker.sock',
  });

  await init(docker, options);
  
  // Setup the DockerEventEmitter
  const stream = new DockerEE(docker);
  stream.on('network.connect', connect(docker, options));
  stream.on('network.disconnect', disconnect(docker, options));
  await stream.start();
})().catch(err => {
  console.log(err.message, err.stack) && process.exit(1)
});

function trim(string, charlist)
{
  if (charlist === undefined) charlist = "\s";
  string = string.replace(new RegExp("^[" + charlist + "]+"), "");
  string = string.replace(new RegExp("[" + charlist + "]+$"), "");
  return string;
}

async function init (docker, options) {
  const containerList = await docker.listContainers();
  // Obtain a list of configuration objects
  options.configList = getConfigurations(docker, containerList);

  if(process.env.DEBUG === 'true'){
    console.dir({options}, {depth:null});
  }

  await renderConfigurations(docker, options.configList);
};

function connect (docker, options) {
  return async (event) => {
    console.dir({NETWORK_CONNECT: event}, {depth:null});

    const containerList = await docker.listContainers();
    // Obtain a list of configuration objects
    options.configList = getConfigurations(docker, containerList);
    await renderConfigurations(docker, options.configList);
    
    console.dir(options, {depth:null});
  };
}

function disconnect (docker, options) {
  return async (event) => {
    console.dir({NETWORK_DISCONNECT: event}, {depth:null});

    const containerList = await docker.listContainers();
    // Obtain a list of configuration objects
    options.configList = getConfigurations(docker, containerList);
    await renderConfigurations(docker, options.configList);

    console.dir(options, {depth:null});
  };
}

function addNetwork(networkList, id, name)
{
  console.log(`Adding Network[${id}] = '${name}'`);
  return {...networkList, [id]: name};
}

function removeNetwork(networkList, id)
{
  console.log(`Removing Network[${id}] = '${networkList[name]}'`);
  const {[id]: removed, ...newList} = networkList;
  return newList;
}

function getConfigurations(docker, containerList)
{
  const list = {};
  const key = 'docker-config-gen';

  for(const container of containerList){
    const name = trim(container.Names[0], '/');
    const input = container.Labels[`${key}.input`];
    const output = container.Labels[`${key}.output`];
    const execString = trim(container.Labels[`${key}.exec`] ?? 'echo no exec action defined', '"');
    let networks = {};

    // You must have both labels to be able to process this container correctly
    if(input === undefined || output === undefined){
      continue;
    }
console.dir({found_networks: container.NetworkSettings.Networks}, {depth:null});
    for(const [name, network] of Object.entries(container.NetworkSettings.Networks)){
      networks = addNetwork(networks, network.NetworkID, name);
    }

    list[container.Id] = {
      name, 
      input: process.env.CONFIG_PATH + input, 
      output: process.env.CONFIG_PATH + output, 
      networks,
      trigger: async () => {
        console.log(`Calling '${execString}' on container '${container.Id}'`);
        let c = docker.getContainer(container.Id);
        let e = await c.exec({Cmd: execString.split(' ')});
        e.start();
      },
    };
  }

  return list;
}

function resolveEnvConfig(env)
{
  return env.reduce((env, e) => {
    const [key, value] = e.split('=');
    return {...env, [key]: value};
  }, {});
}

// FIXME: this is so specific to the nginx-proxy that I should move it into the template "header"
// FIXME: and not have this hardcoded into this project which would make the project overall more generic
function resolveProxyConfig(env, labels)
{
  const defaultConfig = {
    port: 80, 
    path: '/', 
    proto: 'http', 
    root: '/var/www/public',
    serverTokens: '',
    networkTag: 'external',
    httpsMethod: 'redirect',
    hsts: 'max-age=31536000',
  };

  let config = [];

  const virtualField = [
    'VIRTUAL_HOST', 
    'VIRTUAL_PORT', 
    'VIRTUAL_PATH', 
    'VIRTUAL_ROOT', 
    'VIRTUAL_PROTO'
  ];

  const envConfig = {};
  for (const k of virtualField){
    if(env[k] !== undefined){
      envConfig[k.split('_').pop().toLowerCase()] = env[k];
    }
  }

  if(Object.keys(envConfig).length > 0) {
    config = [...[{...defaultConfig, ...envConfig}]];
  }

  let labelConfig = {};
  for (let [key, value] of Object.entries(labels)){
    const [project, route, field] = key.split('.');

    if(project !== 'docker-proxy'){ 
      continue;
    }

    if(labelConfig[route] === undefined){
      labelConfig[route] = {...defaultConfig};
    }

    labelConfig[route][field] = value;
  }

  if(typeof labelConfig['default'] !== 'undefined'){
    for (let [key, value] of Object.entries(labelConfig)){
      labelConfig[key] = {...labelConfig['default'], ...value};
    }
  }

  return [...config, ...Object.values(labelConfig)];
}

function resolveNetworkConfig(settings)
{
  const config = {};

  for (let networkName in settings.Networks){
    config[networkName] = [];
    for (let port in settings.Ports){
      const [num, proto] = port.split('/');
      
      config[networkName].push({
        ip: settings.Networks[networkName].IPAddress,
        port: num,
        proto: proto,
      });
    }
  }

  return config;
}

async function renderConfigurations(docker, configList)
{
  for (const [containerId, config] of Object.entries(configList)) {
    const containerList = {};

    for (const [networkId, networkName] of Object.entries(config.networks)) {
      const network = docker.getNetwork(networkId);
      const networkInspect = await network.inspect();
      
      for (const containerId of Object.keys(networkInspect.Containers)) {
        const container = docker.getContainer(containerId);
        const containerInspect = await container.inspect();

        const env = resolveEnvConfig(containerInspect.Config.Env);
        const config = resolveProxyConfig(env, containerInspect.Config.Labels);
        const name = networkInspect.Containers[containerId].Name;

        if(config.length > 0){
          console.log(`Found Container[${containerId}] = '${name}'`);

          containerList[containerId] = {
            id: containerId,
            name,
            env,
            network: resolveNetworkConfig(containerInspect.NetworkSettings),
            config,
          }
        }
      }
    }

    await renderTemplate(containerList, config.input, config.output);
    config.trigger();
  }
}

async function renderTemplate(containerList, inputFile, outputFile)
{
  if(process.env.DEBUG === 'true'){
    console.dir({
      renderTemplate: {
        inputFile, 
        outputFile, 
        containerList
      }
    }, {depth: null});
  }

  const data = {
    containerList, 
    fs, 
    server: {
      // TODO: Proxy container env var: SSL_POLICY=string, falling back to "Mozilla-Intermediate"
      sslPolicy: "Mozilla-Intermediate",
      // TODO: Proxy container env var: RESOLVERS=string
      resolvers: false,
      // TODO: Proxy container env var ACCESS_LOGS_ENABLED=true|false
      accessLogEnabled: true,
      accessLog: "/var/log/nginx/access.log",
      // TODO: Proxy container env var ENABLE_IPV6=true|false
      enableIpv6: false,
      // TODO: Proxy container env var HTTP_PORT
      httpPort: 80,
      // TODO: Proxy container env var HTTPS_PORT
      httpsPort: 443,
    }
  };
 
  let template = await ejs.renderFile(inputFile, data, {async:true});
  await fs.writeFileSync(outputFile, reformatTemplate(template));
}

function reformatTemplate(template)
{
  const indent = '    ';
  let count = 0;

  // This is really shit and haphazard, but it actually works quite well
  template = template.split("\n").reduce((doc, line) => {
    line = line.trim();

    // do not separate a block away from the comment above it
    if(line.endsWith('{') && doc.length && !doc[doc.length-1].startsWith('#')) {
      doc.push('');
    }

    // if this closes a block, reduce the indentation for the current line
    if(line.startsWith('}')) {
      count--;
    }

    // Add non-empty lines with appropriate indentation
    if(line.length > 0){
      // make sure count never becomes negative
      count = Math.abs(count);
      doc.push(indent.repeat(count) + line);    
    }

    // if this opens a new block, increase the indentation for the next line
    if(line.endsWith('{')) {
      count++;
    }

    // If this closes a block, and the previous line DOES NOT closes a block, add a blank line
    if(line === '}' && doc.length && !doc[doc.length-1].endsWith('}')) {
      doc.push('');
    }
    
    return doc;
  }, []);

  return template.join("\n") + "\n";
}
