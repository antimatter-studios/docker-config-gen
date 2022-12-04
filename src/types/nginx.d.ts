interface NetworkLocation {
    name: string,
    ipAddress: string,
    port: number,
}

interface Upstream {
    name: string,
    networks: NetworkLocation[];
}

interface UpstreamList {
    [key: string]: Upstream,
}

interface Location {
    path: string,
    pathIsRegex: boolean,
    protocol: string,
    upstream: string,
}

interface Server {
    host: string,
    locations: Location[],
}

interface ServerList {
    [key: string]: Server,
}

interface VirtualHost {
    host: string,
    port: number,
    path: string,
    pathIsRegex: boolean,
    protocol: string,
}