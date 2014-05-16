// vi: sw=4 ts=4:

/*

	Mnemonic:	agent
	Abstract:	Manages everything associated with agents. Listens on the well known channel
				for requests from other tegu threads, and manages a separate data channel
				for agent input (none expected at this time.

	Date:		30 April 2014
	Author:		E. Scott Daniels

	Mods:		05 May 2014 : Added ability to receive and process json data from the agent
					and the function needed to process the output from a map_mac2phost request.
					Added ability to send the map_mac2phost request to the agent. 
				13 May 2014 : Added support for exit-dscp value.
*/

package managers

import (
	//"bufio"
	"encoding/json"
	//"flag"
	"fmt"
	//"io/ioutil"
	//"html"
	//"net/http"
	"os"
	"strings"
	//"time"

	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	"forge.research.att.com/gopkgs/connman"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/gopkgs/jsontools"
	//"forge.research.att.com/tegu"
	//"forge.research.att.com/tegu/gizmos"
)

// ----- structs used to bundle into json commands

type action struct {			// specific action
	Atype	string				// something like map_mac2phost, or intermed_queues
	Hosts	[]string			// list of hosts to apply the action to
	Dscps	string				// space separated list of dscp values
}

type agent_cmd struct {			// overall command
	Ctype	string
	Actions []action
}

/*
	Manage things associated with a specific agent
*/
type agent struct {
	id		string
	jcache	*jsontools.Jsoncache				// buffered input resulting in 'records' that are complete json blobs
}

type agent_data struct {
	agents	map[string]*agent					// hash for direct index
}

/*
	Generic struct to unpack json received from an agent
*/
type agent_msg struct {
	Ctype	string			// command type -- should be response, ack, nack etc.
	Rtype	string			// type of response (e.g. map_mac2phost, or specific id for ack/nack)
	Rdata	[]string		// response data
	State	int				// if an ack/nack some state information 
	Vinfo	string			// agent verion (dbugging mostly)
}


/*
	Build an agent and add to our list of agents.
*/
func (ad *agent_data) Mk_agent( aid string ) ( na *agent ) {

	na = &agent{}
	na.id = aid
	na.jcache = jsontools.Mk_jsoncache()

	ad.agents[na.id] = na

	return
}

/*
	Send the message to one agents.
	TODO: randomise this a bit. 
*/
func (ad *agent_data) send2one( smgr *connman.Cmgr,  msg string ) {
	for id := range ad.agents {
		smgr.Write( id, []byte( msg ) )
		return
	}
}

/*
	Send the bytes to one agent.
*/
func (ad *agent_data) sendbytes2one( smgr *connman.Cmgr,  msg []byte ) {
	for id := range ad.agents {
		smgr.Write( id,  msg )
		return
	}
}

/*
	Send the message to all agents.
	TODO: split the hosts list and give each agent a subset of the list
*/
func (ad *agent_data) send2all( smgr *connman.Cmgr,  msg string ) {
	am_sheep.Baa( 2, "sending %d bytes", len( msg ) )
	for id := range ad.agents {
		smgr.Write( id, []byte( msg ) )
	}
}

/*
	Deal with incoming data from an agent. We add the buffer to the cahce
	(all input is expected to be json) and attempt to pull a blob of json
	from the cache. If the blob is pulled, then we act on it, else we 
	assume another buffer or more will be coming to complete the blob
	and we'll do it next time round.
*/
func ( a *agent ) process_input( buf []byte ) {
	var (
		req	agent_msg		// unpacked message struct
	)

	a.jcache.Add_bytes( buf )
	jblob := a.jcache.Get_blob()						// get next blob if ready
	for ; jblob != nil ; {
    	err := json.Unmarshal( jblob, &req )           // unpack the json 

		if err != nil {
			am_sheep.Baa( 0, "ERR: unable to unpack agent_message: %s", err )
			am_sheep.Baa( 2, "ERR: offending json: %s", string( buf ) )
		} else {
			am_sheep.Baa( 1, "%s/%s received from agent", req.Ctype, req.Rtype )
	
			switch( req.Ctype ) {					// "command type"
				case "response":					// response to a request
					if req.State == 0 {
						switch( req.Rtype ) {
							case "map_mac2phost":
								msg := ipc.Mk_chmsg( )
								msg.Send_req( nw_ch, nil, REQ_MAC2PHOST, req.Rdata, nil )		// send into network manager -- we don't expect response
			
							default:	
								am_sheep.Baa( 1, "WRN:  unrecognised response type from agent: %s", req.Rtype )
						}
				} else {
					am_sheep.Baa( 1, "WRN: response for failed command received and ignored: %s", req.Rtype )
				}

				default:
					am_sheep.Baa( 1, "WRN:  unrecognised command type type from agent: %s", req.Ctype )
			}
		}

		jblob = a.jcache.Get_blob()			// get next blob if the buffer completed one and containe a second
	}

	return
}

//-------- request builders -----------------------------------------------------------------------------------------

/*
	Build a request to have the agent generate a mac to phost list and send it to one agent.
*/
func (ad *agent_data) send_mac2phost( smgr *connman.Cmgr, hlist *string ) {
	if hlist == nil || *hlist == "" {
		return
	}
	
/*
	req_str := `{ "ctype": "action_list", "actions": [ { "atype": "map_mac2phost", "hosts": [ `
	toks := strings.Split( *hlist, " " )
	sep := " "
	for i := range toks {
		req_str += sep + `"` + toks[i] +`"`
		sep = ", "
	}

	req_str += ` ] } ] }`
*/

	msg := &agent_cmd{ Ctype: "action_list" }				// create command struct then convert to json
	msg.Actions = make( []action, 1 )
	msg.Actions[0].Atype = "map_mac2phost"
	msg.Actions[0].Hosts = strings.Split( *hlist, " " )
	jmsg, err := json.Marshal( msg )			// bundle into a json string

	if err == nil {
		am_sheep.Baa( 3, "sending mac2phost request: %s", jmsg )
		ad.sendbytes2one( smgr, jmsg )
	} else {
		am_sheep.Baa( 1, "WRN: unable to bundle map2phost request into json: %s", err )
		am_sheep.Baa( 2, "offending json: %s", jmsg )
	}
}

/*
	Build a request to cause the agent to drive the setting of queues and fmods on intermediate bridges.
*/
func (ad *agent_data) send_intermedq( smgr *connman.Cmgr, hlist *string, dscp *string ) {
	if hlist == nil || *hlist == "" {
		return
	}
	
	msg := &agent_cmd{ Ctype: "action_list" }				// create command struct then convert to json
	msg.Actions = make( []action, 1 )
	msg.Actions[0].Atype = "intermed_queues"
	msg.Actions[0].Hosts = strings.Split( *hlist, " " )
	msg.Actions[0].Dscps = *dscp

	jmsg, err := json.Marshal( msg )			// bundle into a json string

	if err == nil {
		am_sheep.Baa( 1, "sending intermediate queue setup request: %s", string( jmsg ) )
		ad.sendbytes2one( smgr, jmsg )
	} else {
		am_sheep.Baa( 1, "creating json intermedq command failed: %s", err )
	}
}
// ---------------- utility ------------------------------------------------------------------------

/*
	Accepts a string of space separated dscp values and returns a string with the values
	approprately shifted so that they can be used by the agent in a flow-mod command.  E.g.
	a dscp value of 40 is shifted to 160. 
*/
func shift_values( list string ) ( new_list string ) {
	new_list = ""
	sep := ""
	toks := strings.Split( list, " " )
	
	for i := range toks {
		n := clike.Atoi( toks[i] )
		new_list += fmt.Sprintf( "%s%d", sep, n<<2 )
		sep = " "
	}

	return
}

// ---------------- main agent goroutine -----------------------------------------------------------

func Agent_mgr( ach chan *ipc.Chmsg ) {
	var (
		port	string = "29055"						// port we'll listen on for connections
		adata	*agent_data
		host_list string = ""
		dscp_list string = "40 41 42"				// list of dscp values that are used to promote a packet to the pri queue in intermed switches
		refresh int64 = 60
	)

	adata = &agent_data{}
	adata.agents = make( map[string]*agent )

	am_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	am_sheep.Set_prefix( "agentmgr" )
	tegu_sheep.Add_child( am_sheep )					// we become a child so that if the master vol is adjusted we'll react too

														// suss out config settings from our section
	if cfg_data["agent"] != nil {
		if p := cfg_data["agent"]["port"]; p != nil {
			port = *p
		}
		if p := cfg_data["agent"]["verbose"]; p != nil {
			am_sheep.Set_level( uint( clike.Atoi( *p ) ) )
		}
		if p := cfg_data["agent"]["refresh"]; p != nil {
			refresh = int64( clike.Atoi( *p ) )
		}
	}
	if cfg_data["default"] != nil {						// we pick some things from the default section too
		if p := cfg_data["default"]["pri_dscp"]; p != nil {			// list of dscp (diffserv) values that match for priority promotion
			dscp_list = *p
		}
	}
	
	dscp_list = shift_values( dscp_list )				// must shift values before giving to agent

														// enforce some sanity on config file settings
	net_sheep.Baa( 1,  "agent_mgr thread started: listening on port %s", port )
net_sheep.Baa( 1, "dscp priority list for intermedate bridges: %s", dscp_list )

	tklr.Add_spot( 2, ach, REQ_MAC2PHOST, nil, 1 );  					// tickle once, very soon after starting, to get a mac translation
	tklr.Add_spot( refresh, ach, REQ_MAC2PHOST, nil, ipc.FOREVER );  	// reocurring tickle to get host mapping 
	tklr.Add_spot( refresh * 2, ach, REQ_INTERMEDQ, nil, ipc.FOREVER );  	// reocurring tickle to ensure intermediate switches are properly set

	sess_chan := make( chan *connman.Sess_data, 1024 )					// channel for comm from agents (buffers, disconns, etc)
	smgr := connman.NewManager( port, sess_chan );
	

	for {
		select {							// wait on input from either channel
			case req := <- ach:
				req.State = nil				// nil state is OK, no error

				am_sheep.Baa( 3, "processing request %d", req.Msg_type )

				switch req.Msg_type {
					case REQ_NOOP:						// just ignore -- acts like a ping if there is a return channel

					case REQ_SENDALL:
						if req.Req_data != nil {
							adata.send2all( smgr,  req.Req_data.( string ) )
						}

					case REQ_SENDONE:
						if req.Req_data != nil {
							adata.send2one( smgr,  req.Req_data.( string ) )
						}

					case REQ_MAC2PHOST:					// send a request for agent to generate  mac to phost map
						if host_list != "" {
							adata.send_mac2phost( smgr, &host_list )
						}

					case REQ_CHOSTLIST:					// a host list from fq-manager
						if req.Req_data != nil {
							host_list = *(req.Req_data.( *string ))
						}

					case REQ_INTERMEDQ:
						req.Response_ch = nil
						if host_list != "" {
							adata.send_intermedq( smgr, &host_list, &dscp_list )
						}

				}

				am_sheep.Baa( 3, "processing request finished %d", req.Msg_type )			// we seem to wedge in network, this will be chatty, but may help
				if req.Response_ch != nil {				// if response needed; send the request (updated) back 
					req.Response_ch <- req
				}


			case sreq := <- sess_chan:		// data from the network
				switch( sreq.State ) {
					case connman.ST_ACCEPTED:		// newly accepted connection; no action 

					case connman.ST_NEW:			// new connection
						a := adata.Mk_agent( sreq.Id )
						am_sheep.Baa( 1, "new agent: %s [%s]", a.id, sreq.Data )
						if host_list != "" {											// immediate request for this 
							adata.send_mac2phost( smgr, &host_list )
							adata.send_intermedq( smgr, &host_list, &dscp_list )
						}
				
					case connman.ST_DISC:
						am_sheep.Baa( 1, "agent dropped: %s", sreq.Id )
						if _, not_nil := adata.agents[sreq.Id]; not_nil {
							delete( adata.agents, sreq.Id )
						} else {
							am_sheep.Baa( 1, "did not find an agent with the id: %s", sreq.Id )
						}
						
					case connman.ST_DATA:
						if _, not_nil := adata.agents[sreq.Id]; not_nil {
							am_sheep.Baa( 2, "data: [%s]  %d bytes received:  first 100b: %s", sreq.Id, len( sreq.Buf ), sreq.Buf[0:100] )
							adata.agents[sreq.Id].process_input( sreq.Buf )
						} else {
							am_sheep.Baa( 1, "data from unknown agent: [%s]  %d bytes ignored:  %s", sreq.Id, len( sreq.Buf ), sreq.Buf )
						}
				}
		}			// end select
	}
}

