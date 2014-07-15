// vi: sw=4 ts=4:

/*

	Mnemonic:	tools
	Abstract:	General functions that probably don't warrent promotion to the forge
				gopkgs library.

	Date:		10 March 2014
	Author:		E. Scott Daniels

	Mods:		13 May 2014 -- Added toks2map function.
*/

package gizmos

import (
	//"bufio"
	//"encoding/json"
	//"flag"
	//"fmt"
	//"io/ioutil"
	//"html"
	//"net/http"
	//"os"
	"strings"
	"time"

	//"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	//"forge.research.att.com/gopkgs/token"
	//"forge.research.att.com/gopkgs/ipc"
)


/*
	Split a string into a start/end UNIX time stamp assuming the string is in one of the 
	following formats:
		+nnnn		start == now	end == now + nnn
		timestamp	start == now	end == timestamp
		ts1-ts2		start == ts1	end == ts2  (start may be adjusted to now if old)

	If the end time value is before the start time value it is set to the start time value.
*/
func Str2start_end( tok string ) ( startt int64, endt int64 ) {
	now := time.Now().Unix()

	if tok[0:1] == "+"	{
		startt = now
		endt  = startt + clike.Atoll( tok )
	} else {
		idx := strings.Index( tok, "-" )			// separate start-end times
		if idx > 0 {
			startt = clike.Atoll( tok[0:idx] )
			endt = clike.Atoll( tok[idx+1:] )

			if startt < now {
				startt = now
			}
		} else {
			startt = now
			endt = clike.Atoll( tok )
		}
	}

	if endt < startt {
		endt = startt
	}

	return
}


/*
	Split two host names of the form host1,host2 or host1-host2 and return the two strings.
*/
func Str2host1_host2( tok string ) ( h1 string, h2 string ) {

	idx := strings.Index( tok, "," )		// alternate form to allow for names like host-1
	if idx > 0 {
		h1 = tok[0:idx]
		h2 = tok[idx+1:]
	} else {
		idx = strings.Index( tok, "-" )		// separate host-host
		if idx > 0 {
			h1 = tok[0:idx]
			h2 = tok[idx+1:]
		} else {
			h1 = tok
			h2 = "any"
		}
	}

	return
}

/*
	Parse a set of tokens passed in, assuming they are name=value pairs, and generate a map. 
	Tokens that are not of the form key=value are ignored.
*/
func Toks2map( toks []string ) ( m map[string]*string ) {
	m = make( map[string]*string )

	for i := range toks {
		t := strings.SplitN( toks[i], "=", 2 )
		
		if len( t ) == 2 {
			m[t[0]] = &t[1]
		}
	}

	return
}

/*
	Mixed tokens (key=value and positional) to map.
	Given an array of tokens (strings), and a list of names, generate a map of tokens 
	referenced by the corresponding name.  If tokens list is longer than the name list
	the remaining tokens are not mapped.  If leading tokens are of the form key=value, 
	then they are mapped directly and tokens after the last key=value pair in the tokens
	array are then mapped in order. Thus splitting the string
		action=parse verbose=true  300  usera userb
	split into tokens, and the names string of "duration u1 u2" would result in a 
	map:
		{ action: "parse", verbose: true, duration: 300, u1: usera, u2: userb }

	TODO: this needs to handle quoted tokens so that "xxx = yyyy" isn't treated as key 
		a value pair.
*/
func Mixtoks2map( toks []string, names string ) ( tmap map[string]*string ) {
	tmap = nil

	nlist := strings.Split( names, " " )
	maxt := len( toks )
	tmap = make( map[string]*string, len( toks ) )

	j := 0											// ensure it lives after loop
	for j = 0; j < maxt; j++ {
		if strings.Index( toks[j], "=" ) < 0 {
			break
		}

		stok := strings.SplitN( toks[j], "=", 2 )
		tmap[stok[0]] = &stok[1]
	}

	for i := range nlist {
		if j >= maxt {
			return
		}

		tmap[nlist[i]] = &toks[j]
		j++
	}

	return
}
	
