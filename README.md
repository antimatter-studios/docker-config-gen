# docker-config-gen
A docker monitoring tool which can generate configurations based on docker containers

# Known Issues

1. Race condition to generate a configuration which hasn't yet appeared in the file system
    - There is a race condition where the docker-config-gen program can get a request to generate a configuration, before the requesting container can write the template meaning it'll fail because the template file does not exist yet, and will need to wait some seconds before it can do that. 
    - I should build some logic which can detect missing template files, watch them and generate them when the file is eventually written

2. Malicious templates could exfiltrate data?
    - What if a malicious container joins the server, passing a template which just dumps the entire container list and all it's environment parameters, etc to a file and exfiltrates it to a remote server. That would be pretty bad. 
    - We need to prevent this from happening somehow. Maybe this is why we can't just pass the entire container list with everything we know to a rando-template-from-some-cool-container-that-asked-for-it(tm) 
    - I think the solution is the concept of "renderers" where each type of file the docker-config-gen project supports, has an associated renderer with it