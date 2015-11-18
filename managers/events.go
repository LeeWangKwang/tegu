// vi: sw=4 ts=4:
/*
 ---------------------------------------------------------------------------
   Copyright (c) 2013-2015 AT&T Intellectual Property

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at:

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
 ---------------------------------------------------------------------------
*/


/*

	Mnemonic:	events.go
	Abstract:	Functions related to event handling (message listeners registered with 
				osif/msgrtr), including various functions that are registered.  Functions 
				registered by each manager thread should start with a prefix which relates 
				it to that manager to avoid confusion. 

	Date:		11 November 2015
	Author:		E. Scott Daniels

	Mods:
*/

package managers

import (
	"fmt"
	"strings"

	"github.com/att/gopkgs/ipc"
	"github.com/att/gopkgs/ipc/msgrtr"
	"github.com/att/tegu/gizmos"
)


// -- event data -------------------------------------------------------------------------

/*
	This datablock is passed to an event message listener (see events.go) with information
	that it needs to do it's job without requiring globals.
*/
type event_handler_data struct {
	req_chan	chan *ipc.Chmsg					// request channel that should be used
}

// ------------ generic/support funcions --------------------------------------------------------------------------------

/*
	Pull the desired things from the event payload and place into a map of strings.
	The field names in the 'what' list are searched for and a missing list string
	which contains the fields that weren't there or couldn't be converted is 
	returned with the map.   An empty ("") missing string indicates no errors.
*/
func payload_2smap( e *msgrtr.Event, what string ) ( m map[string]string, missing_stuff string ) {

	missing_stuff = ""
	tokens := strings.Split( what, " " )
	m = make( map[string]string, len( tokens ) )

	for i := range tokens {
		switch thing := e.Payload[tokens[i]].(type) {
			case string:
				m[tokens[i]] = thing

			case *string:
				if thing == nil {
					m[tokens[i]] = ""
				} else {
					m[tokens[i]] = *thing
				}

			case bool:
				if thing {
					m[tokens[i]] = "true"
				} else {
					m[tokens[i]] = "false"
				}

			case int, int64:
				m[tokens[i]] = fmt.Sprintf( "%d", thing )

			case float64:
				m[tokens[i]] = fmt.Sprintf( "%.3f", thing )

			default:
				net_sheep.Baa( 1, "%s in event payload was buggered or missing", tokens[i] )
				missing_stuff += " " + tokens[i]
		}

	}

	return m, missing_stuff
}

// ------------ network manager event funcions --------------------------------------------------------------------------------

/*
	Network manager event handler which acts on endpoint messages.
	Ldi is the listener data interface variable that was given to the message router when the event was
	registered.
*/
func netev_endpt( e *msgrtr.Event, ldi interface{} ) {

	net_sheep.Baa( 1, "endpt event received; %s payload has %d things", e, len( e.Payload ) )
	
	edata := ldi.( *event_handler_data )		// get refrence to our thread data to use

	payload, missing_stuff := payload_2smap( e, "uuid owner mac ip phost action" )
	if missing_stuff != "" {
		if e.Ack {
			e.Reply( "ERROR", fmt.Sprintf( "event json wasn't complete: payload missing things: %s", missing_stuff ), "" )
		}

		net_sheep.Baa( 1, "event json wasn't complete: payload missing things: %s", missing_stuff )
		return
	}
		
	switch payload["action"] {
		case "add", "mod":
			net_sheep.Baa( 2, "event: adding endpoint: uuid=%s owner=%s mac=%s ip=%s phost=%s", payload["uuid"], payload["owner"], payload["mac"], payload["ip"], payload["phost"] )
			eplist := make( map[string]*gizmos.Endpt, 1 )
			eplist[payload["uuid"]] = gizmos.Mk_endpt( payload["uuid"], payload["phost"], payload["owner"], payload["ip"], payload["mac"], nil, -128 )
			req := ipc.Mk_chmsg( )
			req.Send_req( edata.req_chan, nil, REQ_NEW_ENDPT, eplist, nil )				// send to ourselves to deal with in the main channel processing

		case "del", "delete":
			net_sheep.Baa( 1, "event: deleting endpoint: uuid=%s owner=%s mac=%s ip=%s phost=%s", payload["uuid"], payload["owner"], payload["mac"], payload["ip"], payload["phost"] )

			eplist := make( map[string]*gizmos.Endpt, 1 )
			eplist[payload["uuid"]] = gizmos.Mk_endpt( payload["uuid"], "", payload["owner"], payload["ip"], payload["mac"], nil, -2 )			// if phost is nil/"" then it is deleted
			req := ipc.Mk_chmsg( )
			req.Send_req( edata.req_chan, nil, REQ_NEW_ENDPT, eplist, nil )				// send to ourselves to deal with in the main channel processing

		default:
			if e.Ack {
				e.Reply( "ERROR", "event json wasn't complete: payload missing valid action (add/del/mod)", "" )
			}

			return
	}

	if e.Ack {
		e.Reply( "OK", "Got it", "" )
	}
}

/*
	Network manager event handler which acts on network messages.
*/
func netev_net( e *msgrtr.Event, ldi interface{} ) {
	net_sheep.Baa( 1, "net event received; %s", e )

	//edata := ldi.( *event_handler_data )		// get refrence to our thread data to use
	if e.Ack {
		e.Reply( "OK", "Got it", "" )
	}
}

// ------------ reservation manager funcions --------------------------------------------------------------------------------
