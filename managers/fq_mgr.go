// vi: sw=4 ts=4:

/*

	Mnemonic:	fq_mgr 
	Abstract:	flow/queue manager. This module contains the goroutine that 
				listens on the fq_ch for messages that cause flow mods to be 
				sent (skoogi reservations) and OVS queue commands to be generated.

	Config:		These variables are referenced if in the config file (defaults in parens):
					fqmgr:ssq_cmd     - the command to execute when needing to adjust switch queues  (/opt/app/set_switch_queues)
					fqmgr:queue_check - the frequency (seconds) between checks to see if queues need to be reset (5)
					fqmgr:host_check  - the frequency (seconds) between checks to see  what _real_ hosts open stack reports (180)
					fqmgr:switch_hosts- A space sep list of hosts to set switch queues on; if given then openstack is _not_ queried (no list)
					default:sdn_host  - the host name where skoogi (sdn controller) is running
					
	Date:		29 December 2013
	Author:		E. Scott Daniels

	Mods:		30 Apr 2014 (sd) - Changes to support pushing flow-mods and reservations to an agent. Tegu-lite
				05 May 2014 (sd) - Now sends the host list to the agent manager in addition to keeping a copy
					for it's personal use. 
				12 May 2014 (sd) - Reverts dscp values to 'original' at the egress switch
				19 May 2014 (sd) - Changes to allow floating ip to be supplied as a part of the flow mod.
				07 Jul 2014 - Added support for reservation refresh.
				20 Aug 2014 - Corrected shifting of mdscp value in the match portion of the flowmod (it wasn't being shifted) (bug 210)
				25 Aug 2014 - Major rewrite to send_fmod_agent; now uses the fq_req struct to make it more
					generic and flexible.
				27 Aug 2014 - Small fixes during testing. 
*/

package managers

import (
	//"bufio"
	//"errors"
	"fmt"
	//"io"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/tegu/gizmos"
)

// --- Private --------------------------------------------------------------------------


/*
	We depend on an external script to actually set the queues so this is pretty simple.

	hlist is the space separated list of hosts that the script should adjust queues on
	muc is the max utilisation commitment for any link in the network. 
*/

/*
	Writes the list of queue adjustment information (we assume from net-mgr) to a randomly named
	file in /tmp. Then we invoke the command passed in via cmd_base giving it the file name
	as the only parameter.  The command is expected to delete the file when finished with 
	it.  See netmgr for a description of the items in the list. 

	This is old school and probably will be deprecated soon. 
*/
func adjust_queues( qlist []string, cmd_base *string, hlist *string ) {
	var (
		err error
		cmd_str	string			// final command string (with data file name)
	)

	if hlist == nil {
		hlist = &empty_str
	}

	fname := fmt.Sprintf( "/tmp/tegu_setq_%d_%x_%02d.data", os.Getpid(), time.Now().Unix(), rand.Intn( 10 ) )
	fq_sheep.Baa( 2, "adjusting queues: creating %s will contain %d items", fname, len( qlist ) );

	f, err := os.Create( fname )
	if err != nil {
		fq_sheep.Baa( 0, "ERR: unable to create data file: %s: %s  [TGUFQM000]", fname, err )
		return
	}
	
	for i := range qlist {
		fq_sheep.Baa( 2, "writing queue info: %s", qlist[i] )
		fmt.Fprintf( f, "%s\n", qlist[i] )
	}

	err = f.Close( )
	if err != nil {
		fq_sheep.Baa( 0, "ERR: unable to create data file (close): %s: %s  [TGUFQM001]", fname, err )
		return
	}

	fq_sheep.Baa( 1, "executing: sh %s -d %s %s", *cmd_base, fname, *hlist )
	cmd := exec.Command( shell_cmd, *cmd_base, "-d", fname, *hlist )
	err = cmd.Run()
	if err != nil  {
		fq_sheep.Baa( 0, "ERR: unable to execute set queue command: %s: %s  [TGUFQM002]", cmd_str, err )
	} else {
		fq_sheep.Baa( 1, "queues adjusted via %s", *cmd_base )
	}
}


/*
	Builds one setqueue json request per host and sends it to the agent. If there are 
	multiple agents attached, the individual messages will be fanned out across the 
	available agents, otherwise the agent will just process them sequentially which
	would be the case if we put all hosts into the same message.
*/
func adjust_queues_agent( qlist []string, hlist *string ) {
	var (
		qjson	string			// final full json blob
		qjson_pfx	string		// static prefix
		sep = ""
	)

	fq_sheep.Baa( 1, "adjusting queues:  sending %d queue setting items to agents",  len( qlist ) );

	qjson_pfx = `{ "ctype": "action_list", "actions": [ { "atype": "setqueues", "qdata": [ `

	for i := range qlist {
		fq_sheep.Baa( 2, "queue info: %s", qlist[i] )
		qjson_pfx+= fmt.Sprintf( "%s%q", sep, qlist[i] )
		sep = ", "
	}

	qjson_pfx+= ` ], "hosts": [ `

	hosts := strings.Split( *hlist, " " )
	sep = ""
	for i := range hosts {			// build one request per host and send to agents -- multiple ageents then these will fan out
		qjson = qjson_pfx			// seed the next request with the constant prefix
		qjson += fmt.Sprintf( "%s%q", sep, hosts[i] )

		qjson += ` ] } ] }`
	
		tmsg := ipc.Mk_chmsg( )
		tmsg.Send_req( am_ch, nil, REQ_SENDSHORT, qjson, nil )		// send this as a short request to one agent
	}
}

/*
	DEPRECATED -- send_gfmod_agent to send a flow-mod from a generic structure rather
		than from the specific reservation oriented list. 




	Send a flow-mod request to the agent. If send_all is false, then only ingress/egress
	flow-mods are set; those indicateing queue 1 are silently skipped. This is controlled
	by the queue_type setting in the config file. 

	act_type is either "add" or "del" as this can be used to delete flow-mods when a 
	reservation is canceled. 

	destip is the IP address of the destination if different than either ip1 or ip2. 
	This is used in the case of a split path where there are gateways between and 
	we need to match the mac of the gateway as the dest, but also match the destination
	ip to further limit the sessions which match. This string is assumed to have been
	prepended with the necessary type (-D or -S) as set by the sender of the request.

	tp_sport and tp_dport are the transport port for source and dest.  We assume both 
	UDP and TCP traffic is implied and thus need to generate two fmods when either 
	is set. 

	mdscp is the dscp to match; a value of 0 causes no dscp value to be matched
	wdscp is the dscp value that should be written on the outgoing datagram.

	DSCP values are assumed to NOT have been shifted!


	*** TODO: Eventually calls to this should be changed to use the generic struct and the 
		generic function for generating flow mod requests.
*/
func send_fmod_agent( act_type string, ip1 string, ip2 string, extip string, tp_sport int, tp_dport int, expiry int64, qnum int, sw string, port int, ip2mac map[string]*string, mdscp int, wdscp int, send_all bool ) {
	var (
		host	string
		qjson2	string = ""
	)

	if send_all || qnum != 1 {					// ignore if skipping intermediate and this isn't ingress/egress
		m1 := ip2mac[ip1]						// set with mac addresses
		m2 := ip2mac[ip2]

		if m1 == nil || m2 == nil {
			fq_sheep.Baa( 1, "WRN: unable to set flow mod ip2mac could not be translated for either %s (%v) or %s (%v)  [TGUFQM003]", ip1, m1 == nil, ip2, m2 == nil )
			return
		}

		timeout := expiry - time.Now().Unix()	// figure the timeout and skip if too small
		if timeout > 0 {
			if port <= 0 {						// we'll assume that the switch is actually the phy host and br-int is what needs to be set
				host = sw
				sw = "br-int"
			} else {							// port known, so switch must ben known too
				host = "all"
			}
	
			qjson := `{ "ctype": "action_list", "actions": [ { "atype": "flowmod", "fdata": [ `

			fq_sheep.Baa( 1, "flow-mod: pri=400 tout=%d src=%s dest=%s extip=%s host=%s q=%d mT=%d/%02x aT=%d/%02x tp_ports=%d/%d sw=%s", 
				timeout, *m1, *m2, extip, host, qnum, mdscp, mdscp << 2, wdscp, wdscp << 2, tp_sport, tp_dport, sw )

			qjson += fmt.Sprintf( `"-h %s -t %d -p 400 --match -T %d -s %s -d %s `, host, timeout, mdscp << 2, *m1, *m2 ) 	// MUST always match a dscp value (even 0) to prevent loops on resubmit  (fix #210)
			if extip != "" {
				qjson += fmt.Sprintf( `%s `,  extip )																		// external IP is assumed to be prefixed with the correct -S or -D flag
			}

			action := fmt.Sprintf( `--action -q %d -T %d -R ,0 -N  %s 0xdead %s"`, qnum, wdscp << 2, act_type, sw )			// action will be the same for each set of json
			if tp_sport > 0 || tp_dport > 0 {					// need to match on a transport port too
				qjson2 = qjson									// dup the string
				if tp_sport > 0 {
					qjson += fmt.Sprintf( "-p 6:%d ", tp_sport )		// add in prototype 6 == tcp
					qjson2 += fmt.Sprintf( "-p 17:%d ", tp_sport )		// add in prototype 17 == udp
				}
				if tp_dport > 0 {										// repeat if a dest port was given
					qjson += fmt.Sprintf( "-P 6:%d ", tp_dport )
					qjson2 += fmt.Sprintf( "-P 17:%d ", tp_dport )
				}

				qjson2 += action
				qjson2 += ` ] } ] }`
			}

			qjson += fmt.Sprintf( `--action -q %d -T %d -R ,0 -N  %s 0xdead %s"`, qnum, wdscp << 2, act_type, sw )			// set queue and dscp value, then resub table 0 to match openstack fmod
			qjson += ` ] } ] }`


			tmsg := ipc.Mk_chmsg( )
			fq_sheep.Baa( 2, "1st/only json >>>> %s", qjson )
			tmsg.Send_req( am_ch, nil, REQ_SENDSHORT, qjson, nil )		// send as a short request to one agent

			if qjson2 != "" {
				tmsg := ipc.Mk_chmsg( )
				fq_sheep.Baa( 2, "2nd json >>>> %s", qjson2 )
				tmsg.Send_req( am_ch, nil, REQ_SENDSHORT, qjson, nil )		// send as a short request to one agent
			}
		}
	}
}

/*
	Send a flow-mod to the agent using a generic struct to represnt the match and action criteria.

	The fq_req contains data that are neither match or action specific (priority, expiry, etc) or 
	are single purpose (match or action only) e.g. late binding mac value. It also contains  a set 
	of match and action paramters that are applied depending on where they are found. 
	Data expected in the fq_req:
		Nxt_mac - the mac address that is to be set on the action as dest (steering)
		Expiry  - the timeout for the fmod(s)
		Ip1/2	- The src/dest IP addresses for match (one must be supplied)
		Meta	- The meta value to set/match (both optional)
		Swid	- The switch DPID or host name (ovs) (used as -h option)
		Swport	- The switch port to match (inbound)
		Table	- Table number to put the flow mod into
		Rsub    - A list (space separated) of table numbers to resub to in the order listed.
		Lbmac	- Assumed to be the mac address associated with the switch port when
					switch port is -128. This is passed on the -i option to the 
					agent allowing the underlying interface to do late binding
					of the port based on the mac address of the mbox.
		Pri		- Fmod priority

	TODO: this needs to be expanded to be generic and handle all possible match/action parms
		not just the ones that are specific to res and/or steering.  It will probably need 
		an on-all flag in the main request struct rather than deducing it from parms. 
*/
func send_gfmod_agent( data *Fq_req, ip2mac map[string]*string, hlist *string ) {

	if data == nil {
		return
	}

	if data.Pri <= 0 {
		data.Pri = 100
	}

	timeout := int64( 0 )									// never expiring if expiry isn't given 
	if data.Expiry > 0 {
		timeout = data.Expiry - time.Now().Unix()			// figure the timeout and skip if invalid
	}
	if timeout < 0 {
		fq_sheep.Baa( 1, "timeout for flow-mod was too small, not generated: %d", timeout )
		return
	}

	table := ""
	if data.Table > 0 {
		table = fmt.Sprintf( "-T %d ", data.Table )
	} 

	match_opts := "--match"					// build match options

	if data.Match.Meta != nil {
		if *data.Match.Meta != "" {
			match_opts += " -m " + *data.Match.Meta
		}
	} 

	if data.Match.Swport > 0  {						// valid port
		match_opts += fmt.Sprintf( " -i %d", data.Match.Swport )
	} else {
		if data.Match.Swport == -128 {				// late binding port, we sub in the late binding MAC that was given
			if data.Lbmac != nil {
				match_opts += fmt.Sprintf( " -i %s", *data.Lbmac )
			} else {
				fq_sheep.Baa( 1, "ERR: creating fmod: late binding port supplied, but late binding MAC was nil  [TGUFQM004]" )
			}
		}
	}

	smac := data.Match.Smac								// smac wins if both smac and sip are given
	if smac == nil {
		if data.Match.Ip1 != nil {						// src supplied, match on src
			smac = ip2mac[*data.Match.Ip1]
			if smac == nil {
				fq_sheep.Baa( 0, "ERR: cannot set fmod: src IP did not translate to MAC: %s  [TGUFQM005]", *data.Match.Ip1 )
				return
			}
		}
	}
	if smac != nil {
		match_opts += " -s " + *smac
	}

	dmac := data.Match.Dmac								// dmac wins if both dmac and sip are given
	if dmac == nil {
		if data.Match.Ip2 != nil {						// src supplied, match on src
			dmac = ip2mac[*data.Match.Ip2]
			if dmac == nil {
				fq_sheep.Baa( 0, "ERR: cannot set fmod: dst IP did not translate to MAC: %s  [TGUFQM006]", *data.Match.Ip2 )
				return
			}
		}
	}
	if dmac != nil {
		match_opts += " -d " + *dmac
	}

	if data.Extip != nil  &&   *data.Extip != "" {					// an external IP address must be matched in addition to gw mac
		match_opts += " " + *data.Exttyp + " " + *data.Extip		// caller must set the direction (-S or -D) as we don't know
	}

	if data.Match.Dscp >= 0  {
		match_opts += fmt.Sprintf( " -T %d", data.Match.Dscp << 2 )	// agent expects value shifted off of the TOS bits.
	}


	action_opts := "--action"										// build the action options

	if data.Action.Dmac != nil {						
		action_opts += " -d " + *data.Action.Dmac
	}
	if data.Action.Smac != nil {
		action_opts += " -s " + *data.Action.Smac
	}

	if data.Nxt_mac != nil {								// ??? is this really needed; steering should just set the dest in action
		action_opts += " -d " + *data.Nxt_mac				// change the dest for steering if next hop supplied
	}

	if data.Action.Dscp >= 0  && data.Match.Dscp != data.Action.Dscp {	// no need to set it if it's what we matched on{
		action_opts += fmt.Sprintf( " -T %d", data.Action.Dscp << 2 )	// MUST shift; agent expects dscp to have lower two bits as 0
	}

	if data.Espq != nil && data.Espq.Queuenum >= 0 {
		action_opts += fmt.Sprintf( " -q %d", data.Espq.Queuenum )
	}

	if data.Action.Meta != nil {
		if *data.Action.Meta != "" {
			action_opts += " -m " + *data.Action.Meta
		}
	}

	output := "-N"												// output default to none
	if data.Output != nil {
		switch *data.Output {
			case "none":		output = "-N"
			case "normal":		output = "-n"
			case "drop":		output = "-X"

			default:
				fq_sheep.Baa( 1, "WRN: defaulting to no output: unknown fmod-output type specified: %s  [TGUFQM007]", *data.Output )
		}
	}
	if data.Resub != nil {				 						// action options order may be sensitive; ensure -R is last
		toks := strings.Split( *data.Resub, " " )
		for i := range toks {
			action_opts += " -R ," + toks[i]
		}

		output = "-N"											// for resub there is no output or resub doesn't work (override Output if given)
	}


	action_opts = fmt.Sprintf( "%s %s", action_opts, output )		// set up actions

	base_json := `{ "ctype": "action_list", "actions": [ { "atype": "flowmod", "fdata": [ `

	if data.Swid == nil {											// blast the fmod to all named hosts if a single target is not named
		hosts := strings.Split( *hlist, " " )
		for i := range hosts {
			tmsg := ipc.Mk_chmsg( )									// must have one per since we dont wait for an ack

			json := base_json
			json += fmt.Sprintf( `"-h %s %s -t %d -p %d %s %s add 0x%x %s"`, hosts[i], table, timeout, data.Pri, match_opts, action_opts, data.Cookie, data.Espq.Switch )
			json += ` ] } ] }`
			fq_sheep.Baa( 2, "json: %s", json )
			tmsg.Send_req( am_ch, nil, REQ_SENDSHORT, json, nil )		// send as a short request to one agent
		}
	} else {															// fmod goes only to the named switch
		json := base_json
		json += fmt.Sprintf( `"-h %s -t %d -p %d %s %s add 0x%x %s"`, 
			data.Espq.Switch, timeout, data.Pri, match_opts, action_opts, data.Cookie, *data.Swid )	// Espq.Switch has real name (host) of switch
		json += ` ] } ] }`
		fq_sheep.Baa( 2, "json: %s", json )

		tmsg := ipc.Mk_chmsg( )
		tmsg.Send_req( am_ch, nil, REQ_SENDSHORT, json, nil )		// send as a short request to one agent
	}
}

/*
	Send a newly arrived host list to the agent manager.
*/
func send_hlist_agent( hlist *string ) {
	tmsg := ipc.Mk_chmsg( )
	tmsg.Send_req( am_ch, nil, REQ_CHOSTLIST, hlist, nil )			// push the list; does not expect response back
}

/*
	Send a request to openstack interface for a host list. We will _not_ wait on it 
	and will handle the response in the main loop. 
*/
func req_hosts(  rch chan *ipc.Chmsg ) {
	fq_sheep.Baa( 2, "requesting host list from osif" )

	req := ipc.Mk_chmsg( )
	req.Send_req( osif_ch, rch, REQ_CHOSTLIST, nil, nil )
}

/*
	Send a request to openstack interface for an ip to mac map. We will _not_ wait on it 
	and will handle the response in the main loop. 
*/
func req_ip2mac(  rch chan *ipc.Chmsg ) {
	fq_sheep.Baa( 2, "requesting host list from osif" )

	req := ipc.Mk_chmsg( )
	req.Send_req( osif_ch, rch, REQ_IP2MACMAP, nil, nil )
}


// --- Public ---------------------------------------------------------------------------


/*
	the main go routine to act on messages sent to our channel. We expect messages from the 
	reservation manager, and from a tickler that causes us to evaluate the need to resize 
	ovs queues.

	DSCP values:  Dscp values range from 0-64 decimal, but when described on or by 
		flow-mods are shifted two bits to the left. The send flow mod function will
		do the needed shifting so all values outside of that one funciton should assume/use
		decimal values in the range of 0-64.

*/
func Fq_mgr( my_chan chan *ipc.Chmsg, sdn_host *string ) {

	var (
		uri_prefix	string = ""
		msg			*ipc.Chmsg
		data		[]interface{}			// generic list of data on some requests
		fdata		*Fq_req					// flow-mod request data
		qcheck_freq	int64 = 5
		hcheck_freq	int64 = 180
		host_list	*string					// current set of openstack real hosts
		ip2mac		map[string]*string		// translation from ip address to mac
		switch_hosts *string				// from config file and overrides openstack list if given (mostly testing)
		ssq_cmd		*string					// command string used to set switch queues (from config file)
		send_all	bool = false			// send all flow-mods; false means send just ingress/egress and not intermediate switch f-mods
		dscp		int = 42	 			// generic diffserv value used to mark packets as priority

		//max_link_used	int64 = 0			// the current maximum link utilisation
	)

	fq_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	fq_sheep.Set_prefix( "fq_mgr" )
	tegu_sheep.Add_child( fq_sheep )					// we become a child so that if the master vol is adjusted we'll react too

	// -------------- pick up config file data if there --------------------------------
	if *sdn_host == "" {													// not supplied on command line, pull from config	
		if sdn_host = cfg_data["default"]["sdn_host"];  sdn_host == nil {	// no default; when not in config, then it's turned off and we send to agent
			sdn_host = &empty_str
		}
	}

	if cfg_data["default"]["queue_type"] != nil {					
		if *cfg_data["default"]["queue_type"] == "endpoint" {
			send_all = false
		} else {
			send_all = true
		}
	}

	if cfg_data["fqmgr"] != nil {								// pick up things in our specific setion
		if dp := cfg_data["fqmgr"]["ssq_cmd"]; dp != nil {		// set switch queue command
			ssq_cmd = dp
		}
	
		if p := cfg_data["fqmgr"]["default_dscp"]; p != nil {		// this is a single value and should not be confused with the dscp list in the default section of the config
			dscp = clike.Atoi( *p )
		}

		if p := cfg_data["fqmgr"]["queue_check"]; p != nil {		// queue check frequency from the control file
			qcheck_freq = clike.Atoi64( *p )
			if qcheck_freq < 5 {
				qcheck_freq = 5
			}
		}
	
		if p := cfg_data["fqmgr"]["host_check"]; p != nil {		// frequency of checking for new _real_ hosts from openstack
			hcheck_freq = clike.Atoi64( *p )
			if hcheck_freq < 30 {
				hcheck_freq = 30
			}
		}
	
		if p := cfg_data["fqmgr"]["switch_hosts"]; p != nil {
			switch_hosts = p;
		} 
	
		if p := cfg_data["fqmgr"]["verbose"]; p != nil {
			fq_sheep.Set_level(  uint( clike.Atoi( *p ) ) )
		}
	}
	// ----- end config file munging ---------------------------------------------------

	//tklr.Add_spot( qcheck_freq, my_chan, REQ_SETQUEUES, nil, ipc.FOREVER );  	// tickle us every few seconds to adjust the ovs queues if needed

	if switch_hosts == nil {
		tklr.Add_spot( 2, my_chan, REQ_CHOSTLIST, nil, 1 );  						// tickle once, very soon after starting, to get a host list
		tklr.Add_spot( hcheck_freq, my_chan, REQ_CHOSTLIST, nil, ipc.FOREVER );  	// tickles us every once in a while to update host list
		fq_sheep.Baa( 2, "host list will be requested from openstack every %ds", hcheck_freq )
	} else {
		host_list = switch_hosts
		fq_sheep.Baa( 0, "static host list from config used for setting OVS queues: %s", *host_list )
	}

	if sdn_host != nil  &&  *sdn_host != "" {
		uri_prefix = fmt.Sprintf( "http://%s", *sdn_host )
	} 

	fq_sheep.Baa( 1, "flowmod-queue manager is running, sdn host: %s", *sdn_host )
	for {
		msg = <- my_chan					// wait for next message 
		msg.State = nil						// default to all OK
		
		fq_sheep.Baa( 3, "processing message: %d", msg.Msg_type )
		switch msg.Msg_type {
			case REQ_GEN_FMOD:							// generic fmod; just pass it along w/o any special handling
				if msg.Req_data != nil {
					fdata = msg.Req_data.( *Fq_req ); 		// pointer at struct with all of our expected goodies
					send_gfmod_agent( fdata,  ip2mac, host_list )
				}

			case REQ_IE_RESERVE:						// proactive ingress/egress reservation flowmod
				fdata = msg.Req_data.( *Fq_req ); 		// user view of what the flow-mod should be

				if uri_prefix != "" {						// an sdn controller -- skoogi -- is enabled
					msg.State = gizmos.SK_ie_flowmod( &uri_prefix, *fdata.Match.Ip1, *fdata.Match.Ip2, fdata.Expiry, fdata.Espq.Queuenum, fdata.Espq.Switch, fdata.Espq.Port )

					if msg.State == nil {					// no error, no response to requestor
						fq_sheep.Baa( 2,  "proactive reserve successfully sent: uri=%s h1=%s h2=%s exp=%d qnum=%d swid=%s port=%d dscp=%d",  
									uri_prefix, fdata.Match.Ip1, fdata.Match.Ip2, fdata.Expiry, fdata.Espq.Queuenum, fdata.Espq.Switch, fdata.Espq.Port )
						msg.Response_ch = nil
					} else {
						// do we need to suss out the id and mark it failed, or set a timer on it,  so as not to flood reqmgr with errors?
						fq_sheep.Baa( 1,  "ERR: proactive reserve failed: uri=%s h1=%s h2=%s exp=%d qnum=%d swid=%s port=%d  [TGUFQM008]",  
									uri_prefix, fdata.Match.Ip1, fdata.Match.Ip2, fdata.Expiry, fdata.Espq.Queuenum, fdata.Espq.Switch, fdata.Espq.Port )
					}
				} else {
					rdscp := 0
					if fdata.Preserve_dscp > 0 {					
						rdscp = fdata.Preserve_dscp				// reservation supplied value that is to be preserved
					}
																// q-lite generates three flowmods  in each direction
					if send_all || fdata.Espq.Queuenum > 1 {	// if sending all fmods, or this has a non-intermediate queue
						cdata := fdata.Clone()					// copy so we can alter w/o affecting sender's copy
						if cdata.Espq.Port == -128 {			// we'll assume in this case that the switch given is the host name and we need to set the switch to br-int
							swid := "br-int"
							cdata.Swid = &swid
						}

						resub_list := "10 0"						// resub to table 10 to pick up meta character, then to table 0 to hit openstack junk
						cdata.Resub = &resub_list
				
						meta := "0x00/0x02"						// match-value/mask; match only when meta 0x2 is 0
						cdata.Match.Meta = &meta

						if fdata.Dir_in  {						// inbound to this switch we need to revert dscp from our settings to the 'origianal' settings
							if cdata.Single_switch {
								cdata.Match.Dscp =  -1													// no dscp adjustment when it is a single switch
								send_gfmod_agent( cdata,  ip2mac, host_list )
							} else {
								cdata.Match.Dscp = dscp								// our value used to keep priority as it wanders through the net
								cdata.Action.Dscp = rdscp							// translate our value to the one in the reservation (0 by default)
								send_gfmod_agent( cdata,  ip2mac, host_list )

																	// TODO:  these translations need to be loaded from config, and a loop used to generate all fmods
								cdata := cdata.Clone()				// clone the clone so we pick up our changes from above, and modify for next fmod
								cdata.Match.Dscp = 40
								cdata.Action.Dscp = 32
								send_gfmod_agent( cdata,  ip2mac, host_list )
	
								cdata = cdata.Clone()
								cdata.Match.Dscp = 41
								cdata.Action.Dscp = 46
								send_gfmod_agent( cdata,  ip2mac, host_list )
							}
						} else {													// outbound from this switch we need to translate dscp values to our values
							if cdata.Single_switch {
								cdata.Match.Dscp =  -1								// no dscp processing for a single switch
								send_gfmod_agent( cdata,  ip2mac, host_list )
							} else {
								cdata.Match.Dscp = rdscp							// match the dscp provided on the reservation (0 by default)
								cdata.Action.Dscp = dscp							// translate to our special value which gets priority on intermediate switches
	
								send_gfmod_agent( cdata,  ip2mac, host_list )
	
																	// TODO:  these translations need to be loaded from config, and a loop used to generate all fmods
								cdata = cdata.Clone()				// copy for next request, fill in and send
								cdata.Match.Dscp = 32
								cdata.Action.Dscp = 40
								send_gfmod_agent( cdata,  ip2mac, host_list )
	
								cdata = cdata.Clone()				// copy for next request, fill in and send
								cdata.Match.Dscp = 46
								cdata.Action.Dscp = 41
								send_gfmod_agent( cdata,  ip2mac, host_list )
							}
						}
					}

					msg.Response_ch = nil
				}

			case REQ_RESERVE:								// send a reservation to skoogi
				data = msg.Req_data.( []interface{} ); 		// msg data expected to be array of interface: h1, h2, expiry, queue h1/2 must be IP addresses
				if uri_prefix != "" {
					fq_sheep.Baa( 2,  "msg to reserve: %s %s %s %d %d",  uri_prefix, data[0].(string), data[1].(string), data[2].(int64), data[3].(int) )
					msg.State = gizmos.SK_reserve( &uri_prefix, data[0].(string), data[1].(string), data[2].(int64), data[3].(int) )
				} else {
					fq_sheep.Baa( 1, "reservation not sent, no sdn-host defined:  %s %s %s %d %d",  uri_prefix, data[0].(string), data[1].(string), data[2].(int64), data[3].(int) )
				}

			case REQ_SETQUEUES:								// request from reservation manager which indicates something changed and queues need to be reset
				qlist := msg.Req_data.( []interface{} )[0].( []string )
				if ssq_cmd != nil {
					adjust_queues( qlist, ssq_cmd, host_list ) 	// if writing to a file and driving a local script
				} else {
					adjust_queues_agent( qlist, host_list )		// if sending json to an agent
				}

			case REQ_CHOSTLIST:								// this is tricky as it comes from tickler as a request, and from openstack as a response, be careful!
				msg.Response_ch = nil;						// regardless of source, we should not reply to this request

				if msg.State != nil || msg.Response_data != nil {				// response from ostack if with list or error
					if  msg.Response_data.( *string ) != nil {
						host_list = msg.Response_data.( *string )
						send_hlist_agent( host_list )							// send to agent_manager
						fq_sheep.Baa( 1, "host list received from osif: %s", *host_list )
					} else {
						fq_sheep.Baa( 0, "WRN: no  data from openstack; expected host list string  [TGUFQM009]" )
					}
				} else {
					fq_sheep.Baa( 2, "requesting lists from osif" )
					req_hosts( my_chan )					// send requests to osif for data
					req_ip2mac( my_chan )
				}

			case REQ_IP2MACMAP:								// caution: this  comes as a response; the request is generated by chostlist processing so we need 1 tickle
				msg.Response_ch = nil;						// regardless of source, we should not reply to this request

				if msg.State != nil || msg.Response_data != nil {				// response from ostack if with list or error
					if  msg.Response_data != nil {
						ip2mac = msg.Response_data.( map[string]*string )
						fq_sheep.Baa( 1, "ip2mac translation received from osif: %d elements", len( ip2mac ) )
					} else {
						fq_sheep.Baa( 0, "WRN: no  data from openstack; expected ip2mac translation map  [TGUFQM010]" )
					}
				}

			default:
				fq_sheep.Baa( 1, "unknown request: %d", msg.Msg_type )
				msg.Response_data = nil
				if msg.Response_ch != nil {
					msg.State = fmt.Errorf( "unknown request (%d)", msg.Msg_type )
				} 
		}

		fq_sheep.Baa( 3, "processing message complete: %d", msg.Msg_type )
		if msg.Response_ch != nil {			// if a reqponse channel was provided
			fq_sheep.Baa( 3, "sending response: %d", msg.Msg_type )
			msg.Response_ch <- msg			// send our result back to the requestor
		}
	}
}
