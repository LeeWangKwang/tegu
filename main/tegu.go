// vi: sw=4 ts=4:

/*

	Mnemonic:	tegu
	Abstract:	The middle layer that sits between the cQoS and the openflow controller (Skoogi)
				providing an API that allows for the establishment and removal of network
				reserviations.

				Command line flags:
					-C config	-- config file that provides openstack credentials and maybe more
					-c chkpt	-- checkpoint file, last set of reservations
					-f host:port -- floodlight (SDNC) host:port
					-p port		-- tegu listen port (4444)
					-s cookie	-- super cookie
					-v			-- verbose mode

	Date:		20 November 2013
	Author:		E. Scott Daniels

	Mods:		20 Jan 2014 : added support to allow a single VM in a reservation (VMname,any)
							+nnn time now supported on reservation request.
				10 Mar 2014 : converted to per-path queue setting (ingress/egress/middle queues)
				13 Mar 2014 : Corrected 'bug' with setting pledges where both hosts connect to the 
							same switch. (bug was that it wasn't yet implemented.)

	Trivia:		http://en.wikipedia.org/wiki/Tupinambis
*/

package main

import (
	//"bufio"
	//"encoding/json"
	"flag"
	"fmt"
	//"io/ioutil"
	//"html"
	//"net/http"
	"os"
	//"strings"
	"sync"
	//"time"

	//"forge.research.att.com/gopkgs/clike"
	//"forge.research.att.com/gopkgs/token"
	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/tegu/managers"
	"forge.research.att.com/tegu/gizmos"
)

var (
	sheep *bleater.Bleater
)


func usage( version string ) {
	fmt.Fprintf( os.Stdout, "tegu %s\n", version )
	fmt.Fprintf( os.Stdout, "usage: tegu [-C config-file] [-c ckpt-file] [-f floodlight-host] [-p api-port] [-s super-cookie] [-v]\n" )
}

func main() {
	var (
		version		string = "v2.0/13184"
		cfg_file	*string  = nil
		api_port	*string			// command line option vars must be pointers
		verbose 	*bool
		needs_help 	*bool
		fl_host		*string
		super_cookie *string
		chkpt_file	*string

		// various comm channels for threads
		nw_ch	chan *ipc.Chmsg		// network graph manager 
		rmgr_ch	chan *ipc.Chmsg		// reservation manager 
		osif_ch chan *ipc.Chmsg		// openstack interface
		fq_ch chan *ipc.Chmsg			// flow queue manager

		wgroup	sync.WaitGroup
	)

	sheep = bleater.Mk_bleater( 1, os.Stderr )
	sheep.Set_prefix( "tegu-main" )
	sheep.Add_child( gizmos.Get_sheep( ) )			// since we don't directly initialise the gizmo environment we ask for its sheep

	needs_help = flag.Bool( "?", false, "show usage" )

	chkpt_file = flag.String( "c", "", "check-point-file" )
	cfg_file = flag.String( "C", "", "configuration-file" )
	fl_host = flag.String( "f", "", "floodlight_host:port" )
	api_port = flag.String( "p", "29444", "api_port" )
	super_cookie = flag.String( "s", "", "admin-cookie" )
	verbose = flag.Bool( "v", false, "verbose" )

	flag.Parse()									// actually parse the commandline

	if *needs_help {
		usage( version )
		os.Exit( 0 )
	}

	if( *verbose ) {
		sheep.Set_level( 1 )
	}
	sheep.Baa( 1, "tegu %s started", version )
	sheep.Baa( 1, "http api is listening on: %s", *api_port )

	if *super_cookie == "" {							// must have something and if not supplied this is probably not guessable without the code
		x := "20030217"	
		super_cookie = &x
	}

	nw_ch = make( chan *ipc.Chmsg )					// create the channels that the threads will listen to
	fq_ch = make( chan *ipc.Chmsg, 128 )			// reqmgr will spew requests expecting a response (asynch) only if there is an error, so channel must be buffered
	rmgr_ch = make( chan *ipc.Chmsg, 256 );			// buffered to allow fq to send errors; should be more than fq buffer size to prevent deadlock
	osif_ch = make( chan *ipc.Chmsg )

	err := managers.Initialise( cfg_file, nw_ch, rmgr_ch, osif_ch, fq_ch )		// set up package environment
	if err != nil {
		sheep.Baa( 0, "ERR: unable to initialise: %s\n", err ); 
		os.Exit( 1 )
	}

	go managers.Res_manager( rmgr_ch, super_cookie ); 						// manage the reservation inventory
	go managers.Network_mgr( nw_ch, fl_host )								// manage the network graph

	if *chkpt_file != "" {
		my_chan := make( chan *ipc.Chmsg )
		req := ipc.Mk_chmsg( )
	
		req.Send_req( rmgr_ch, my_chan, managers.REQ_LOAD, chkpt_file, nil )
		req = <- my_chan												// block until the file is loaded

		if req.State != nil {
			sheep.Baa( 0, "ERR: unable to load checkpoint file: %s: %s\n", *chkpt_file, req.State )
			os.Exit( 1 )
		}
	}

	go managers.Fq_mgr( fq_ch, fl_host ); 
	go managers.Osif_mgr( osif_ch )										// openstack interface
	go managers.Http_api( api_port, nw_ch, rmgr_ch )				// finally, turn on the HTTP interface after _everything_ else is running.

	wgroup.Add( 1 )					// forces us to block forever since no goroutine gets the group to dec when finished (they dont!)
	wgroup.Wait( )
	os.Exit( 0 )
}

