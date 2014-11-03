// vi: sw=4 ts=4:

/*

	Mnemonic:	res_mgr
	Abstract:	Manages the inventory of reservations. 
				We expect it to be executed as a goroutine and requests sent via a channel.
	Date:		02 December 2013
	Author:		E. Scott Daniels

	CFG:		These config file variables are used when present:
				resmgr:ckpt_dir	- name of the directory where checkpoint data is to be kept (/var/lib/tegu)
				FWIW: /var/lib/tegu selected based on description: http://www.tldp.org/LDP/Linux-Filesystem-Hierarchy/html/var.html


	TODO:		need a way to detect when skoogie/controller has been reset meaning that all
				pushed reservations need to be pushed again. 

				need to check to ensure that a VM's IP address has not changed; repush 
				reservation if it has and cancel the previous one (when skoogi allows drops)

	Mods:		03 Apr 2014 (sd) : Added endpoint flowmod support.
				30 Apr 2014 (sd) : Enhancements to send flow-mods and reservation request to agents (Tegu-light)
				13 May 2014 (sd) : Changed to support exit dscp value in reservation.
				18 May 2014 (sd) : Changes to allow cross tenant reservations.
				19 May 2014 (sd) : Changes to support using destination floating IP address in flow mod.
				07 Jul 2014 (sd) : Changed to send network manager a delete message when deleteing a reservation
						rather than depending on the http manager to do that -- possible timing issues if we wait.
						Added support for reservation refresh.
				29 Jul 2014 : Change set user link cap such that 0 is a valid value, and -1 will delete. 
				08 Sep 2014 (sd) : Fixed bugs with tcp oriented proto steering.
*/

package managers

import (
	"bufio"
	//"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	"forge.research.att.com/gopkgs/chkpt"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/tegu/gizmos"
)

//var (  NO GLOBALS HERE; use globals.go )

// --------------------------------------------------------------------------------------

/*
	Manages the reservation inventory
*/
type Inventory struct {
	cache		map[string]*gizmos.Pledge		// cache of pledges 
	ulcap_cache	map[string]int					// cache of user limit values (max value)
	chkpt		*chkpt.Chkpt
}

// --- Private --------------------------------------------------------------------------

/*
	Encapsulate all of the current reservations into a single json blob.
*/
func ( i *Inventory ) res2json( ) (json string, err error) {
	var (
		sep 	string = ""
	)

	err = nil;
	json = `{ "reservations": [ `

	for _, p := range i.cache {
		if ! p.Is_expired( ) {
			json += fmt.Sprintf( "%s%s", sep, p.To_json( ) )
			sep = ","
		}
	}

	json += " ] }"

	return
}

/*
	Given a name, send a request to the network manager to translate it to an IP address.
*/
func name2ip( name *string ) ( ip *string ) {
	ip = nil

	if name == nil || *name == "" {
		return 
	}

	ch := make( chan *ipc.Chmsg );	
	defer close( ch )									// close it on return
	msg := ipc.Mk_chmsg( )
	msg.Send_req( nw_ch, ch, REQ_GETIP, name, nil )
	msg = <- ch
	if msg.State == nil {					// success
		ip = msg.Response_data.( *string )
		if ip == nil {						// nil string we'll  assume that it was already in ip form
			ip = name
		}
	} else {
		ip = name					// on error assume it's an IP address
	}

	return
}

/*
	Given a name, get host info (IP, mac, switch-id, switch-port) from network.
*/
func get_hostinfo( name *string ) ( *string, *string, *string, int ) {

	if name != nil  &&  *name != "" {
		ch := make( chan *ipc.Chmsg );	
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_HOSTINFO, name, nil )		// get host info string (mac, ip, switch)
		req = <- ch
		if req.State == nil {
			htoks := strings.Split( req.Response_data.( string ), "," )					// results are: ip, mac, switch-id, switch-port; all strings
			return &htoks[0], &htoks[1], &htoks[2], clike.Atoi( htoks[3] )
		}
	}

	return nil, nil, nil, 0
}

/*
	Given an ip address send a request to network manager to request the physical host.
*/
func get_physhost( ip *string ) ( *string ) {

	if ip != nil  &&  *ip != "" {
		ch := make( chan *ipc.Chmsg );	
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_GETPHOST, ip, nil )
		req = <- ch
		if req.State == nil {
			return req.Response_data.( *string )
		}
	}

	return nil 
}

/*
	Handles a response from the fq-manager that indicates the attempt to send a proactive ingress/egress flowmod to skoogi
	has failed.  Issues a warning to the log, and resets the pushed flag for the associated reservation.
*/
func (i *Inventory) failed_push( msg *ipc.Chmsg ) {
	fq_data := msg.Req_data.( []interface{} ) 		// data that was passed to fq_mgr (we'll dig out pledge id
	pid := fq_data[FQ_ID].( string )

	rm_sheep.Baa( 1, "WRN: proactive ie reservation failed, pledge marked unpushed: %s", pid )
	p := i.cache[pid]
	if p != nil {
		p.Reset_pushed()
	}
}

/*
	Checks to see if any reservations expired in the recent past (seconds). Returns true if there were. 
*/
func (i *Inventory) any_concluded( past int64 ) ( bool ) {

	for _, p := range i.cache {									// run all pledges that are in the cache
		if p != nil  &&  p.Concluded_recently( past ) {			// pledge concluded within past seconds
				return true
		}
	}

	return false
}

/*
	Checks to see if any reservations became active between (now - past) and the current time, or will become
	active between now and now + future seconds. (Past and future are number of seconds on either side of 
	the current time to check and are NOT timestamps.)
*/
func (i *Inventory) any_commencing( past int64, future int64 ) ( bool ) {

	for _, p := range i.cache {							// run all pledges that are in the cache
		if p != nil  &&  (p.Commenced_recently( past ) || p.Is_active_soon( future ) ) {	// will activate between now and the window
				return true
		}
	}

	return false
}


/*
	Generate the flow mod that is placed into table 90-something for steering. 
*/
func table9x_fmods( rname *string ) {
		fq_match := &Fq_parms{
			Swport:	-1,								// port 0 is valid, so we need something that is ignored if not set later
			Tpdport: -1,
			Tpsport: -1,
			Meta:	&empty_str,
		}

		mstr := "0x02/0x02"							// sets the meta value
		fq_action := &Fq_parms{
			Meta:	&mstr,
			Tpdport: -1,
			Tpsport: -1,
		}

		fq_data := &Fq_req {							// fq-mgr request data
			Id:		rname,
			Expiry:	0,
			Match: 	fq_match,
			Action: fq_action,
			Table:	90,
		}

		msg := ipc.Mk_chmsg()
		msg.Send_req( fq_ch, nil, REQ_ST_RESERVE, fq_data, nil )			// no response right now -- eventually we want an asynch error
}

/*
	Generate flow-mod requests to the fq-manager for a given src,dest pair and list of 
	middleboxes.  This assumes that the middlebox list has been reversed if necessary. 
	Either source (ep1) or dest (ep2) may be nil which indicates a "to any" or "from any"
	intention. 

	DANGER:  We generate the flowmods in reverse order which _should_ generate the highest 
			priority f-mods first. This is absolutely necessary to prevent packet loops
			on the switches which can happen if a higher priority rule isn't in place 
			that would cause the lower priority rule to be skipped over. 

	TODO: add transport layer port support
*/
func steer_fmods( ep1 *string, ep2 *string, mblist []*gizmos.Mbox, expiry int64, rname *string, proto *string, forward bool ) {
	var (
		fq_data *Fq_req
		fq_match *Fq_parms
		fq_action *Fq_parms
		mb	*gizmos.Mbox							// current middle box being worked with (must span various blocks)
	)

	if expiry < 5 {									// refuse if too short
		return
	}

	mstr := "0x00/0x02"								// meta data match string; match if mask 0x02 is not set
	nmb := len( mblist )
	for i := 0; i < nmb; i++ {						// check value of each in list and bail if any are nil
		if mblist[i] == nil {
			rm_sheep.Baa( 1, "IER: steer_fmods: unexpected nil mb i=%d nmb=%d", i,  nmb )
			return
		}
	}

	for i :=  nmb -1; i >= 0;  i-- {					// backward direction ep2->ep1
		resub := "90 0"									// we resubmit to table 10 to set our meta data and then resub to 0 to catch openstack rules

		if i == nmb - 1 {								// for last mb we need a rule that causes steering to be skipped based on mb mac
			mb = mblist[i]
			fq_match = &Fq_parms{						// new structs for each since they sit in fq manages quwue
				Swport:	-1,								// port 0 is valid, so we need something that is ignored if not set later
				Meta:	&mstr,
				Ip1:  	ep1,
				Tpdport: -1,
				Tpsport: -1,
			}

			fq_action = &Fq_parms{
				Resub: &resub,							// resubmit to table 90 to set meta info, then to 0 to get tunnel matches
			}

			fq_data = &Fq_req {							// generate data with just what needs to be there
				Pri:	300,
				Id:		rname,
				Expiry:	expiry,
				Match:	fq_match,
				Action:	fq_action,
			}

			if proto != nil {								// set the protocol match port dest in forward direction, src in reverse
				toks := strings.Split( *proto, ":" );
				fq_data.Protocol = &toks[0]
				if forward {
					fq_data.Match.Tpdport = clike.Atoi( toks[1] )
				} else {
					fq_data.Match.Tpsport = clike.Atoi( toks[1] )
				}
			}

			if ep1 != nil {									// if source is a specific address, then we need only one 300 rule
				fq_data.Match.Ip1 = nil												// there is no source to match at this point
				fq_data.Match.Smac = nil
				fq_data.Match.Ip2 = ep2
				fq_data.Swid, fq_data.Match.Swport = mb.Get_sw_port( )	// specific switch and input port needed for this fmod
				fq_data.Lbmac = mb.Get_mac()							// fqmgr will need the mac if the port is late binding
			} else {													// if no specific src, 100 rule lives on each switch, so we must put a 300 on each too
				fq_data.Swid = nil										// force to all switches
				fq_data.Match.Smac = mb.Get_mac()						// src for 300 is the last mbox
			}

			rm_sheep.Baa( 1, "write final fmod: %s", fq_data.To_json() )

			msg := ipc.Mk_chmsg()
			msg.Send_req( fq_ch, nil, REQ_ST_RESERVE, fq_data, nil )			// final flow-mod from the last middlebox out
		}

		fq_match = &Fq_parms{
			Swport:	-1,								// port 0 is valid, so we need something that is ignored if not set later
			Meta:	&mstr,
		}

		fq_action = &Fq_parms{
			Resub: &resub,							// resubmit to table 10 to set meta info, then to 0 to get tunnel matches
		}

		fq_data = &Fq_req {						// fq-mgr request data
			Id:		rname,
			Expiry:	expiry,
			Match: 	fq_match,
			Action: fq_action,
			Protocol: proto,
		}
		fq_data.Match.Ip1 = ep1
		fq_data.Match.Ip2 = ep2

		if proto != nil {								// set the protocol match port dest in forward direction, src in reverse
			toks := strings.Split( *proto, ":" );
			fq_data.Protocol = &toks[0]
			if forward {
				fq_data.Match.Tpdport = clike.Atoi( toks[1] )
			} else {
				fq_data.Match.Tpsport = clike.Atoi( toks[1] )
			}
		}
	
		if i == 0 {									// push the ingress rule (possibly to all switches)
			fq_data.Pri = 100

			mb = mblist[i]
			if ep1 != nil {
				_, _, fq_data.Swid, _ = get_hostinfo( ep1 )		// if a specific src host supplied, get it's switch and we'll land only one flow-mod on it
			} else {
				fq_data.Swid = nil								// if ep1 is undefined (all), then we need a f-mod on all switches to handle ingress case
			}
			fq_data.Action.Tpsport = -1							// invalid port when writing to all too
			fq_data.Nxt_mac = mb.Get_mac( )
			rm_sheep.Baa( 2, "write ingress fmod: %s", fq_data.To_json() )

			msg := ipc.Mk_chmsg()
			msg.Send_req( fq_ch, nil, REQ_ST_RESERVE, fq_data, nil )			// no response right now -- eventually we want an asynch error
		} else {																// push fmod on the switch that connects the previous mbox matching packets from it and directing to next mbox

			mb = mblist[i-1] 													// pull previous middlebox which will be the source and define the swtich and port for this rule
			fq_data.Swid, fq_data.Match.Swport = mb.Get_sw_port( )	 			// specific switch and input port needed for this fmod
			fq_data.Lbmac = mb.Get_mac()										// fqmgr will need the mac if the port is late binding (-128)
			fq_data.Match.Smac = nil											// we match based on input port and dest mac, so no need for this
			fq_data.Match.Ip1 = nil												// and no need for the source ip which fqmanager happily translates to a mac

			fq_data.Pri = 200						// priority for intermediate flow-mods
			mb = mblist[i]	 						// now safe to get next middlebox in the list
			fq_data.Nxt_mac = mb.Get_mac( )
			rm_sheep.Baa( 2, "write intemed fmod: %s", fq_data.To_json() )

			msg := ipc.Mk_chmsg()
			msg.Send_req( fq_ch, nil, REQ_ST_RESERVE, fq_data, nil )			// flow mod for each intermediate link in backwards direction
		}
	}
}

/*
	Push the fmod requests to fq-mgr for a steering resrvation. 
*/
func push_st_reservation( p *gizmos.Pledge, rname string, ch chan *ipc.Chmsg ) {

	ep1, ep2, _, _, _, conclude, _, _ := p.Get_values( )		// hosts, ports and expiry are all we need
	now := time.Now().Unix()

	ep1 = name2ip( ep1 )										// we work only with IP addresses; sets to nil if "" (L*)
	ep2 = name2ip( ep2 )

	table9x_fmods( &rname )

	nmb := p.Get_mbox_count()
	mblist := make( []*gizmos.Mbox, nmb ) 
	for i := range mblist {
		mblist[i] = p.Get_mbox( i )
	}
	steer_fmods( ep1, ep2, mblist, conclude - now, &rname, p.Get_proto(), true )			// set forward fmods

	nmb--
	for i := range mblist {											// build middlebox list in reverse
		mblist[nmb-i] = p.Get_mbox( i )
	}
	steer_fmods( ep2, ep1, mblist, conclude - now, &rname, p.Get_proto(), false )			// set backward fmods

	p.Set_pushed()
}

/*
	Runs the list of reservations in the cache and pushes out any that are about to become active (in the 
	next 15 seconds).  

	Returns the number of reservations that were pushed.
*/
func (i *Inventory) push_reservations( ch chan *ipc.Chmsg ) ( npushed int ) {
	var (
		//fq_data	[]interface{}			// local work space to organise data for fq manager
		//fq_sdata	[]interface{}		// copy of data at time message is sent so that it 'survives' after msg sent and this continues to update fq_data
		//msg		*ipc.Chmsg
		//ip2		*string					// the ip ad

		push_count	int = 0
		pend_count	int = 0
		pushed_count int = 0
	)


	rm_sheep.Baa( 2, "pushing reservations, %d in cache", len( i.cache ) )
	for rname, p := range i.cache {							// run all pledges that are in the cache
		if p != nil  &&  ! p.Is_pushed() {
			if p.Is_active() || p.Is_active_soon( 15 ) {	// not pushed, and became active while we napped, or will activate in the next 15 seconds
				if push_count <= 0 {
					rm_sheep.Baa( 1, "pushing proactive reservations" )
				}
				push_count++

				switch p.Get_ptype() {
					case gizmos.PT_BANDWIDTH:
							push_bw_reservation( p, rname, ch )

					case gizmos.PT_STEERING:
							push_st_reservation( p, rname, ch )
				}
			} else {
				pend_count++
			}
		} else {
			pushed_count++
		}
	}

	if push_count > 0 || rm_sheep.Would_baa( 2 ) {			// bleat if we pushed something, or if higher level is set in the sheep
		rm_sheep.Baa( 1, "push_reservations: %d pushed, %d pending, %d already pushed", push_count, pend_count, pushed_count )
	}

	return pushed_count
}

/*
	Turn pause mode on for all current reservations and reset their push flag so thta they all get pushed again.
*/
func (i *Inventory) pause_on( ) {
	for _, p := range i.cache {
		p.Pause( true )					// also reset the push flag		
	}
}

/*
	Turn pause mode off for all current reservations and reset their push flag so thta they all get pushed again.
*/
func (i *Inventory) pause_off( ) {
	for _, p := range i.cache {
		p.Resume( true )					// also reset the push flag		
	}
}

/*
	Run the set of reservations in the cache and write any that are not expired out to the checkpoint file.  
	For expired reservations, we'll delete them if they test positive for extinction (dead for more than 120
	seconds).
*/
func (i *Inventory) write_chkpt( ) {

	err := i.chkpt.Create( )
	if err != nil {
		rm_sheep.Baa( 0, "CRI: resmgr: unable to create checkpoint file: %s", err )
		return
	}

	for nm, v := range i.ulcap_cache {							// write out user link capacity limits that have been set
		fmt.Fprintf( i.chkpt, "ucap: %s %d\n", nm, v ) 			// we'll check the overall error state on close
	}

	for key, p := range i.cache {
		s := p.To_chkpt()		
		if s != "expired" {
			fmt.Fprintf( i.chkpt, "%s\n", s ) 					// we'll check the overall error state on close
		} else {
			if p.Is_extinct( 120 ) && p.Is_pushed( ) {			// if really old and extension was pushed, safe to clean it out
				rm_sheep.Baa( 1, "extinct reservation purged: %s", key )
				delete( i.cache, key )
			}
		}
	} 

	ckpt_name, err := i.chkpt.Close( )
	if err != nil {
		rm_sheep.Baa( 0, "CRI: resmgr: checkpoint write failed: %s: %s", ckpt_name, err )
	} else {
		rm_sheep.Baa( 1, "resmgr: checkpoint successful: %s", ckpt_name )
	}
}

/*
	Opens the filename passed in and reads the reservation data from it. The assumption is that records in 
	the file were saved via the write_chkpt() function and are json pledges.  We will drop any that 
	expired while 'sitting' in the file. 
*/
func (i *Inventory) load_chkpt( fname *string ) ( err error ) {
	var (
		rec		string
		nrecs	int = 0
		p		*gizmos.Pledge
		my_ch	chan	*ipc.Chmsg
		req		*ipc.Chmsg
	)

	err = nil
	my_ch = make( chan *ipc.Chmsg )
	defer close( my_ch )									// close it on return

	f, err := os.Open( *fname )
	if err != nil {
		return
	}
	defer	f.Close( )

	br := bufio.NewReader( f )
	for ; err == nil ; {
		nrecs++
		rec, err = br.ReadString( '\n' )
		if err == nil  {
			switch rec[0:5] {
				case "ucap:":
					toks := strings.Split( rec, " " )
					if len( toks ) == 3 {
						i.add_ulcap( &toks[1], &toks[2] )
					}

				default:
					p = new( gizmos.Pledge )
					p.From_json( &rec )
		
					if  p.Is_expired() {
						rm_sheep.Baa( 1, "resmgr: ckpt_load: ignored expired pledge: %s", p.To_str() )
					} else {
		
						req = ipc.Mk_chmsg( )
						req.Send_req( nw_ch, my_ch, REQ_RESERVE, p, nil )
						req = <- my_ch									// should be OK, but the underlying network could have changed
		
						if req.State == nil {						// reservation accepted, add to inventory
							err = i.Add_res( p )
						} else {
		
							rm_sheep.Baa( 0, "ERR: resmgr: ckpt_laod: unable to reserve for pledge: %s", p.To_str() )
						}
					}
			}
		}
	}

	if err == io.EOF {
		err = nil
	}

	rm_sheep.Baa( 1, "read %d records from checkpoint file: %s", nrecs, *fname )
	return
}

/*
	Given a host name, return all pledges that involve that host as a list. 
	Currently no error is detected and the list may be nill if there are no pledges.
*/
func (inv *Inventory) pledge_list(  vmname *string ) ( []*gizmos.Pledge, error ) {

	if len( inv.cache ) <= 0 {
		return nil, nil
	}

	plist := make( []*gizmos.Pledge, len( inv.cache ) )
	i := 0
	for _, p := range inv.cache {
		if p.Has_host( vmname ) {
			plist[i] = p
			i++
		}
	}

	return plist[0:i], nil
}

/*
	Set the user link capacity and forward it on to the network manager. We expect this
	to be a request from the far side (user/admin) or read from the chkpt file so 
	the value is passed as a string (which is also what network wants too.
*/
func (inv *Inventory) add_ulcap( name *string, sval *string ) {
	val := clike.Atoi( *sval )

	pdata := make( []*string, 2 )		// parameters for message to network
	pdata[0] = name
	pdata[1] = sval

	if val >= 0 && val < 101 {
		rm_sheep.Baa( 2, "adding user cap: %s %d", *name, val )
		inv.ulcap_cache[*name] = val

		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, nil, REQ_SETULCAP, pdata, nil ) 				// push into the netwok environment

	} else {
		if val == -1 {
			delete( inv.ulcap_cache, *name )
			req := ipc.Mk_chmsg( )
			req.Send_req( nw_ch, nil, REQ_SETULCAP, pdata, nil ) 				// push into the netwok environment
		} else {
			rm_sheep.Baa( 1, "user link capacity not set %d is out of range (1-100)", val )
		}
	}
}

// --- Public ---------------------------------------------------------------------------
/*
	constructor
*/
func Mk_inventory( ) (inv *Inventory) {

	inv = &Inventory { } 

	inv.cache = make( map[string]*gizmos.Pledge, 2048 )		// initial size is not a limit
	inv.ulcap_cache = make( map[string]int, 64 )

	return
}

/*
	Stuff the pledge into the cache erroring if the pledge already exists.
*/
func (inv *Inventory) Add_res( p *gizmos.Pledge ) (state error) {
	state = nil
	id := p.Get_id()
	if inv.cache[*id] != nil {
		state = fmt.Errorf( "reservation already exists: %s", *id )
		return
	}

	inv.cache[*id] = p

	rm_sheep.Baa( 1, "resgmgr: added reservation: %s", p.To_chkpt() )
	return
}

/*
	Return the reservation that matches the name passed in provided that the cookie supplied
	matches the cookie on the reservation as well.  The cookie may be either the cookie that 
	the user supplied when the reservation was created, or may be the 'super cookie' admin
	'root' as you will, which allows access to all reservations.
*/
func (inv *Inventory) Get_res( name *string, cookie *string ) (p *gizmos.Pledge, state error) {
	
	state = nil
	p = inv.cache[*name]
	if p == nil {
		state = fmt.Errorf( "cannot find reservation: %s", *name )
		return
	}

	if ! p.Is_valid_cookie( cookie ) &&  *cookie != *super_cookie {
		rm_sheep.Baa( 2, "resgmgr: denied fetch of reservation: cookie supplied (%s) didn't match that on pledge %s", *cookie, *name )
		p = nil
		state = fmt.Errorf( "not authorised to access or delete reservation: %s", *name )
		return
	}

	rm_sheep.Baa( 2, "resgmgr:: fetched reservation: %s", p.To_str() )
	return
}

/*
	Looks for the named reservation and deletes it if found. The cookie must be either the 
	supper cookie, or the cookie that the user supplied when the reservation was created.
	Deletion is affected by reetting the expiry time on the pledge to now + a few seconds. 
	This will cause a new set of flow-mods to be sent out with an expiry time that will
	take them out post haste and without the need to send "delete" flow-mods out. 

	This function sends a request to the network manager to delete the related queues. This
	must be done here so as to prevent any issues with the loosely coupled management of 
	reservation and queue settings.  It is VERY IMPORTANT to delete the reservation from 
	the network perspective BEFORE the expiry time is reset.  If it is reset first then 
	the network splits timeslices based on the new expiry and queues end up dangling.
*/
func (inv *Inventory) Del_res( name *string, cookie *string ) (state error) {

	p, state := inv.Get_res( name, cookie )
	if p != nil {
		rm_sheep.Baa( 2, "resgmgr: deleted reservation: %s", p.To_str() )
		state = nil

		ch := make( chan *ipc.Chmsg )	
		defer close( ch )										// close it on return
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_DEL, p, nil )			// delete from the network point of view
		req = <- ch											// wait for response from network
		state = req.State

		p.Set_expiry( time.Now().Unix() + 15 )				// set the expiry to 15s from now which will force it out
		p.Reset_pushed()									// force push of flow-mods that reset the expiry
	} else {
		rm_sheep.Baa( 2, "resgmgr: unable to delete reservation: not found: %s", *name )
	}

	return
}


/*
	delete all of the reservations provided that the cookie is the super cookie. If cookie
	is a user cookie, then deletes all reservations that match the cookie.
*/
func (inv *Inventory) Del_all_res( cookie *string ) ( ndel int ) {
	var	(
		plist	[]*string			// we'll create a list to avoid deletion issues with range
		i		int
	)

	ndel = 0
	
	plist = make( []*string, len( inv.cache ) )
	for _, pledge := range inv.cache {
		plist[i] = pledge.Get_id()
		i++
	}

	for _, pname := range plist {
		rm_sheep.Baa( 2, "delete all attempt to delete: %s", *pname )
		err := inv.Del_res( pname,  cookie )
		if err == nil {
			ndel++;
			rm_sheep.Baa( 1, "delete all deleted reservation %s", *pname )
		} else {
			rm_sheep.Baa( 1, "delete all skipped reservation %s", *pname )
		}
	}

	rm_sheep.Baa( 1, "delete all deleted %d reservations %s", ndel )
	return
}


/*
	Pulls the reservation from the inventory. Similar to delete, but not quite the same.
	This will clone the pledge. The clone is expired and left in the inventory to force
	a reset of flowmods. The network manager is sent a request to delete the queues 
	assocaited with the path and the path is removed from the original pledge. The orginal
	pledge is returned so that it can be used to generate a new set of paths based on the 
	hosts, expiry and bandwidth requirements of the initial reservation. 

	Unlike the get/del functions, this is meant for internal support and does not
	require a cookie. 

	It is important to delete the reservation from the network manager point of view 
	BEFORE the expiry is reset. If expiry is set first then the network manager will
	cause queue timeslices to be split on that boundary leaving dangling queues. 
*/
func (inv *Inventory) yank_res( name *string ) ( p *gizmos.Pledge, state error) {

	state = nil
	p = inv.cache[*name]
	if p != nil {
		rm_sheep.Baa( 2, "resgmgr: yanked reservation: %s", p.To_str() )
		cp := p.Clone( *name + ".yank" )				// clone but DO NOT set conclude time until after network delete!

		inv.cache[*name + ".yank"] = cp					// insert the cloned pledge into the inventory

		inv.cache[*name] = nil							// yank original from the list
		delete( inv.cache, *name )
		p.Set_path_list( nil )							// no path list for this pledge 	

		ch := make( chan *ipc.Chmsg )	
		defer close( ch )									// close it on return
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_DEL, cp, nil )			// delete from the network point of view
		req = <- ch											// wait for response from network
		state = req.State

														// now safe to set these
		cp.Set_expiry( time.Now().Unix() + 1 )			// force clone to be expired
		cp.Reset_pushed( )								// force it to go out again
	} else {
		state = fmt.Errorf( "no reservation with name: %s", *name )
		rm_sheep.Baa( 2, "resgmgr: unable to yank, no reservation with name: %s", *name )
	}

	return
}

//---- res-mgr main goroutine -------------------------------------------------------------------------------

/*
	Executes as a goroutine to drive the resevration manager portion of tegu. 
*/
func Res_manager( my_chan chan *ipc.Chmsg, cookie *string ) {

	var (
		inv	*Inventory
		msg	*ipc.Chmsg
		ckptd	string
		last_qcheck	int64				// time that the last queue check was made to set window
		queue_gen_type = REQ_GEN_EPQMAP
	)

	super_cookie = cookie				// global for all methods

	rm_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	rm_sheep.Set_prefix( "res_mgr" )
	tegu_sheep.Add_child( rm_sheep )					// we become a child so that if the master vol is adjusted we'll react too

	p := cfg_data["default"]["queue_type"]				// lives in default b/c used by fq-mgr too
	if p != nil {
		if *p == "endpoint" {
			queue_gen_type = REQ_GEN_EPQMAP
		} else {
			queue_gen_type = REQ_GEN_QMAP
		}
	}

	cdp := cfg_data["resmgr"]["chkpt_dir"] 
	if cdp == nil {
		ckptd = "/var/lib/tegu/resmgr"							// default directory and prefix
	} else {
		ckptd = *cdp + "/resmgr"							// add prefix to directory in config
	}

	p = cfg_data["resmgr"]["verbose"]
	if p != nil {
		rm_sheep.Set_level(  uint( clike.Atoi( *p ) ) )
	}

	inv = Mk_inventory( )
	inv.chkpt = chkpt.Mk_chkpt( ckptd, 10, 90 )

	last_qcheck = time.Now().Unix()
	tklr.Add_spot( 2, my_chan, REQ_PUSH, nil, ipc.FOREVER )		// push reservations to skoogi just before they go live
	tklr.Add_spot( 1, my_chan, REQ_SETQUEUES, nil, ipc.FOREVER )	// drives us to see if queues need to be adjusted
	tklr.Add_spot( 180, my_chan, REQ_CHKPT, nil, ipc.FOREVER )		// tickle spot to drive us every 180 seconds to checkpoint

	rm_sheep.Baa( 3, "res_mgr is running  %x", my_chan )
	for {
		msg = <- my_chan					// wait for next message
		
		rm_sheep.Baa( 3, "processing message: %d", msg.Msg_type )
		switch msg.Msg_type {
			case REQ_NOOP:			// just ignore

			case REQ_ADD:
				p := msg.Req_data.( *gizmos.Pledge )	
				msg.State = inv.Add_res( p )
				msg.Response_data = nil

			case REQ_CHKPT:
				rm_sheep.Baa( 3, "invoking checkpoint" )
				inv.write_chkpt( )

			case REQ_DEL:											// user initiated delete -- requires cookie
				data := msg.Req_data.( []*string )					// assume pointers to name and cookie
				if *data[0] == "all" {
					inv.Del_all_res( data[1] )
					msg.State = nil
				} else {
					msg.State = inv.Del_res( data[0], data[1] )
				}

				msg.Response_data = nil

			case REQ_GET:											// user initiated get -- requires cookie
				data := msg.Req_data.( []*string )					// assume pointers to name and cookie
				msg.Response_data, msg.State = inv.Get_res( data[0], data[1] )

			case REQ_LIST:											// list reservations	(for a client)
				msg.Response_data, msg.State = inv.res2json( )

			case REQ_LOAD:								// load from a checkpoint file
				data := msg.Req_data.( *string )		// assume pointers to name and cookie
				msg.State = inv.load_chkpt( data )
				msg.Response_data = nil
				rm_sheep.Baa( 1, "checkpoint file loaded" )
	
			case REQ_PAUSE:
				msg.State = nil							// right now this cannot fail in ways we know about 
				msg.Response_data = ""
				inv.pause_on()
				rm_sheep.Baa( 1, "pausing..." )

			case REQ_RESUME:
				msg.State = nil							// right now this cannot fail in ways we know about 
				msg.Response_data = ""
				inv.pause_off()

			case REQ_SETQUEUES:							// driven about every second to reset the queues if a reservation state has changed
				now := time.Now().Unix()
				if now > last_qcheck  &&  inv.any_concluded( now - last_qcheck ) || inv.any_commencing( now - last_qcheck, 0 ) {
					rm_sheep.Baa( 1, "reservation state change detected, requesting queue map from net-mgr" )
					tmsg := ipc.Mk_chmsg( )
					tmsg.Send_req( nw_ch, my_chan, queue_gen_type, time.Now().Unix(), nil )		// get a queue map; when it arrives we'll push to fqmgr
				}
				last_qcheck = now

			case REQ_PUSH:								// driven every few seconds to push new reservations
				inv.push_reservations( my_chan )

			case REQ_PLEDGE_LIST:						// generate a list of pledges that are related to the given VM
				msg.Response_data, msg.State = inv.pledge_list(  msg.Req_data.( *string ) ) 

			case REQ_SETULCAP:							// user link capacity; expect array of two string pointers (name and value)
				data := msg.Req_data.( []*string )
				inv.add_ulcap( data[0], data[1] )
				inv.write_chkpt( )

			// CAUTION: the requests below come back as asynch responses rather than as initial message
			case REQ_IE_RESERVE:						// an IE reservation failed
				msg.Response_ch = nil					// immediately disable to prevent loop
				inv.failed_push( msg )			// suss out the pledge and mark it unpushed

			case REQ_GEN_QMAP:							// response caries the queue map that now should be sent to fq-mgr to drive a queue update
				fallthrough

			case REQ_GEN_EPQMAP:
				rm_sheep.Baa( 1, "received queue map from network manager" )
				msg.Response_ch = nil											// immediately disable to prevent loop
				fq_data := make( []interface{}, 1 )
				fq_data[FQ_QLIST] = msg.Response_data 
				tmsg := ipc.Mk_chmsg( )
				tmsg.Send_req( fq_ch, nil, REQ_SETQUEUES, fq_data, nil )		// send the queue list to fq manager to deal with
				
			case REQ_YANK_RES:										// yank a reservation from the inventory returning the pledge and allowing flow-mods to purge
				if msg.Response_ch != nil {
					msg.Response_data, msg.State = inv.yank_res( msg.Req_data.( *string ) )
				}

			default:
				rm_sheep.Baa( 0, "WRN: res_mgr: unknown message: %d", msg.Msg_type )
				msg.Response_data = nil
				msg.State = fmt.Errorf( "res_mgr: unknown message (%d)", msg.Msg_type )
		}

		rm_sheep.Baa( 3, "processing message complete: %d", msg.Msg_type )
		if msg.Response_ch != nil {			// if a response channel was provided
			msg.Response_ch <- msg			// send our result back to the requestor
		}
	}
}
