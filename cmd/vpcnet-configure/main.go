package main

func main() {
	// I configure nodes (daemonset)

	// find ourself in the kubernetes api
	//     https://stackoverflow.com/questions/35008011/kubernetes-how-do-i-know-what-node-im-on

	// watch for interfaces/ips defined on the node

	// for each interface, configure the bridge (OS-specific)

	// for the IP's, write them out to the host path where the ipam can see them

	// Poll for current running pods, delete lock files for gone pods
}
