#!/usr/bin/env python3
# :vi ts=4 sw=4 :

'''
    Mneminic:   afm_tests.py
    Abstract:   Automated flow-mod tests. This script is given two VM names (the related OS_
                environment variables are assumed to already be set) and it will create 
                reservations using those two VMs. A third parameter, an external IP address
                is also required on the command line and is used for external reservations.
                The IP address does not have to be real as only the value in the flow mod
                is checked, but should be if any parallel connectivity testing is to 
                be done as these reservations exist.  The script attempts to create a 
                reservation for all possible combinations of reservations which include:
                    - with each dscp type, both global and regular
                    - with and without TCP/UDP port
                    - with and without VLAN designation
                    - oneway

                By default each reservation is set for 30 seconds, but this time can be 
                changed from the command line to allow for additional parallel tests to 
                be executed from the VMs directly.

                This script expects the following:
                    + tegu_req, tegu_osdig and tegu_os2dig are in the PATH
                      (os2dig is required only until neutron supports a python3 library
                      and the code there can be migrated into tegu_osdig.)
                    + ssh to all physical hosts in the cluster can be done without
                      needing a password (key authenticiation)
                    + the script verify_4x_fmods.ksh is on each physical host
                      and the path is supplied via an option to this script or it is
                      in /tmp. 
                    + the only reservations active for the VMs supplied are the ones
                      created by this script.  If other reservations for either of the
                      VMs are present the results may be incorrect.

    Date:       21 Oct 2015
    Author:     E. Scott Daniels

    Requirements:   tegu_osdig must be in the path
'''

import subprocess
import sys
import time

# ------ classes ---------------------------------------------------------------------
class Endpt:
    def __init__( self, epid, mac=None, ip=None ):
        self.epid = epid
        self.mac = mac
        self.ip = ip

    def __str__( self ):
        return "[%s %s %s]" % (self.epid, self.mac, self.ip)

class Vm:
    def __init__( self, name, tuple=None ):
        self.name = name
        self.endpts = {}

    def __str__( self ):
        s = "%s" % self.name
        for epid in self.endpts:
            s += " %s" % self.endpts[epid] 
        return s

    def Add_endpt( self, tuple ):
        tokens = tuple.split( "," )
        self.endpts[tokens[0]] = Endpt( tokens[0], tokens[1], tokens[2] )

    def Get_endpt( self, type=4 ):
        '''
            Returns the endpoint with the IP address type. Randomly picks the 
            endpoint if there are more than one with the same ip type.
        '''
        for ep in self.endpts.values():
            if type == 6:
                if ep.ip.find( ":" ) >= 0:                       # assume IPv6
                    return ep.id, ep.mac, ep.ip
            else:
                if ep.ip.find( "." ) >= 0:                       # ensure type 4
                    return ep.epid, ep.mac, ep.ip
        #end

        return None, None, None

class Test_info:
    '''
        Constant info that will span multiple tests so it can be passed as a single unit
        without overloading global variables
    '''
    def __init__( self, vm1, vm2, ip1, ip2, mac1, mac2, phost1, phost2, rtr_mac, res_time, vcmd ):
        self.vm1 = vm1                  # vm names
        self.vm2 = vm2
        self.ip1 = ip1                  # vm ip addresses
        self.ip2 = ip2
        self.mac1 = mac1                # vm mac adddresses
        self.mac2 = mac2
        self.rtr_mac = rtr_mac             # router mac address
        self.phost1 = phost1            # physical hosts where vms live
        self.phost2 = phost2
        self.res_time = res_time        # reservation time
        self.vcmd = vcmd                # verification command


# ------- support functions ----------------------------------------------------------

def bleat( str ):
    if verbose:
        print( str )

def dig_router( epid ):
    '''
        Given an endpoint id, find the router we believee to be on the same network
    '''
    stdout = subprocess.getoutput( "tegu_os2dig routers %s" % epid )
    tokens = stdout.split( " ", 2 )
    return tokens[0]
#end

def send_reservation( res_time, vm1, ip1, vm2, ip2, dtype, exip=None, oneway=False ):
    '''
        send off a reservation to tegu, and print results if in verbose mode
    '''

    if oneway:
        if exip == None:
            cmd = "%s -T   owreserve 3M +%d %%t/%%p/%s@%s,!//%s cookie %s" % (tr, res_time, vm1, ip1, exip, dtype)
        else:
            print( "ERROR: internal mishap: oneway reservation without external IP address" )
            exit( 1 )
        #end
    else:
        if exip == None:
            cmd = "%s -T   reserve 3M +%d %%t/%%p/%s@%s,%%t/%%p/%s@%s cookie %s" % (tr, res_time, vm1, ip1, vm2, ip2, dtype)
        else:
            cmd = "%s -T   reserve 3M +%d %%t/%%p/%s@%s,!//%s cookie %s" % (tr, res_time, vm1, ip1, exip, dtype)
        #end
    #end

    bleat( "%s" % cmd )
    stdout = subprocess.getoutput( cmd )
    if stdout.find( "OK" ) < 0:
        print( "abort: reservation failed" )
        if not verbose:
            print( "cmd = (%s)" % cmd )
        print( stdout )
        exit( 1 )

    if show_results:
        print( stdout )
#end

def verify_fmods( phost, dscp_value, rmac, lmac, exip=None, lport=None, rport=None, vlan=None, keep=False, vcmd="fmod_ver.ksh" ):
    '''
        Run the command on the remote physical host to verify flowmods
    '''
    opts = ""
    if exip != None:
        opts += " -e %s" % exip
        
    if vlan != None:
        opts += " -v %s" % vlan
    
    if keep:
        opts += " -k"

    if lport != None:
        opts += " -pl %s" % lport

    if rport != None:
        opts += " -pr %s" % rport

    opts += " -l %s -r %s" % (lmac, rmac)
    opts += " -m %s" % dscp_value

    print( "" )
    print( "verification on %s" % phost )
    cmd = "ssh tegu@%s PATH=%s:'$PATH' %s %s" % ( phost, path, vcmd, opts)
    bleat( "running: %s" % cmd )
    stdout = subprocess.getoutput( cmd )
    if not stdout.find( "PASS" ) >= 0:
        print( stdout )
    else:
        if show_results:
            print( stdout )
            print( "" )
        else:
            print( "PASS: flow_mods verified on %s" % phost )
        #end
    #end
#end

def run_one_test( tinfo, port1=None, port2=None, exip=None, vlan=None, oneway=False ):
    '''
        Run a single test and then verify
    '''
    if dtype[0:7] == "global_":
        keep = True
    else:
        keep = False

    if port1 != None:                       # add ports to ip address if supplied
        pip1 = "%s:%d" % (tinfo.ip1, port1)
    else:
        pip1 = tinfo.ip1

    if port2 != None:
        pip2 = "%s:%d" % (tinfo.ip2, port2)
    else:
        pip2 = tinfo.ip2

    if vlan != None:                       # add vlan if supplied
        pip1 = "%s{%d}" % vlan1

    print( "running test: %s" % desc )
    send_reservation( tinfo.res_time, tinfo.vm1, pip1, tinfo.vm2, pip2, dtype, exip, oneway )                 # it hard aborts if not accepted, so return is always good
    print( "reservation accepted; waiting 5 seconds before verifying" )
    time.sleep( 5 )

    if exip == None:
        verify_fmods( tinfo.phost1, dvalue, tinfo.mac2, tinfo.mac1, exip=exip, lport=port1, rport=port2, vlan=vlan, keep=keep, vcmd=tinfo.vcmd )    # verify on h1's host
    else:
        verify_fmods( tinfo.phost1, dvalue, tinfo.rtr_mac, tinfo.mac1, exip=exip, lport=port1, rport=port2, vlan=vlan, keep=keep, vcmd=tinfo.vcmd ) # for external, remote is router mac

    if not (oneway or exip != None):
        verify_fmods( tinfo.phost2, dvalue, tinfo.mac1, tinfo.mac2, exip=exip, lport=port2, rport=port1, vlan=vlan, keep=keep, vcmd=tinfo.vcmd )    # verify on h2's host only if not external/oneway

    pause( tinfo.res_time + 5 )
    if show_results:
        print( "" )
    #end
#end

def map_phosts( ):
    '''
        send a req to tegu (I hate that tegu is nothing more than a database now)
        to find the physical location of each endpoint. Returns a map keyed on 
        endpoint id.
    '''

    stdout = subprocess.getoutput( "%s -T listhosts" % (tr) )
    phost = None
    epid = None
    map = {}
    stdout = stdout[:-1]                          # ditch trailing newline
    for rec in stdout.split( "\n" ):
        rec = rec.strip()                       # ditch lead/trail blanks since split doesn't ignore them
        tokens = rec.split( " " )
        if tokens[0][0:7] == "details":
                epid = None
                phost = None

        elif len( tokens ) >= 3:
            if tokens[0] == "epid":
                if phost == None:
                    epid = tokens[-1]
                else:
                    map[tokens[-1]] = phost
                    phost = None
                    epid = None
            elif tokens[0] == "switch":
                if epid == None:
                    phost = tokens[-1]
                else:
                    map[epid] = tokens[-1]
                    epid = None
                    phost = None
                #end
            #end
        #end

    return map

def pause( ptime ):
    nap = 5
    while ptime > 0:
        if ptime < 15:
            nap = 1

        print( "paused for %4d more seconds" % ptime,  end="\r" )

        time.sleep( nap )
        ptime -= nap
    #end

    print( "                                       " )  # don't assume xterm escape codes work
            

# ------------------------------------------------------------------------------------

def usage( argv0 ):
    print( '''
    usage: %s  [-h tegu-host:port] [-p verifcation-path] [-t res-time] [-V] [-v] vmname1 vmname2 external-ip
           %s  {-?|--help}
''' % (argv0, argv0) ) 



verbose = False
argi = 1
argc = len( sys.argv )
argv0 = sys.argv[0]
res_time = 30                   # minimal (default) reservation time
tegu_host = "-h localhost"      # tegu instance (-h overrides)
vscript_path = "/tmp"           # where we expect the verification script (-p overrieds)
path="/tmp/tegu_b"              # where we expect tegu agent scripts on each phost
show_results = False            # -V disables supression of some of the results 

while argi < argc and sys.argv[argi][0] == "-":
    if sys.argv[argi] == "-h":
        argi += 1
        tegu_host = "-h " + sys.argv[argi]

    elif sys.argv[argi] == "-p":
        argi += 1
        vscript_path = sys.argv[argi]

    elif sys.argv[argi] == "-t":
        argi += 1
        res_time = int( sys.argv[argi] )

    elif sys.argv[argi] == "-v":
        verbose = True
        
    elif sys.argv[argi] == "-V":
        show_results = True
        
    elif sys.argv[argi] == "-?" or sys.argv[argi] == "--help":
        usage( argv0 )
        exit( 0 )

    else:
        print( "unrecognised option: %s" % sys.argv[argi] )
        exit( 1 )
    #end

    argi += 1
#end

tr="ksh  ../system/tegu_req.ksh %s" % tegu_host		# tegu req command

if argc - argi < 3:                     # 3 positional parms required
    print( "missing positional parameters" )
    usage( argv0 )
    exit( 1 )

if res_time < 30:
    print( "-t option sets reservation time unacceptably small: %d" % res_time )
    exit( 1 )
    
vm1 = sys.argv[argi]
vm2 = sys.argv[argi+1]
exip = sys.argv[argi+2]

print( "getting physical host locations from Tegu" )
ep2phost = map_phosts()                         # map to find phys host based on endpoint

print( "getting endpoint information from openstack: %s %s" % (vm1, vm2) )
vms = {}
stdout = subprocess.getoutput( "tegu_osdig -v -a epid %s %s" % (vm1, vm2) ) # dig out info on vms given
for rec in stdout.split( "\n" ):
    rec = rec[:-1]                              # ditch trailing newline
    tokens = rec.split( " " )                   # split into vnmane then tuples of epid,mac,ip
    vm = Vm( tokens[0][:-1] )                   # chop trailing colon from name
    vms[vm.name] = vm

    for i in range( 1, len( tokens ) ):
        vm.Add_endpt( tokens[i] )
    #end
#end
        
epid1, mac1, ip1 = vms[vm1].Get_endpt()          # get one of the ipv4 endpoints from each vm
epid2, mac2, ip2 = vms[vm2].Get_endpt()
phost1 = ep2phost[epid1]
phost2 = ep2phost[epid2]

print( "getting router mac address" )
rtr_mac = dig_router( epid1 )

bleat( "vm1: %s  %s  %s  %s  %s" % (vm1, epid1, mac1, ip1, phost1) )
bleat( "vm2: %s  %s  %s  %s  %s" % (vm2, epid2, mac2, ip2, phost2) )
bleat( "router: %s" % rtr_mac )

verify_cmd = vscript_path + "/fmod_ver.ksh"
dscp_values = { "voice": 184, "control": 104, "data": 72, "global_voice": 184, "global_control": 104, "global_data": 72 }

tinfo = Test_info( vm1, vm2, ip1, ip2, mac1, mac2, phost1, phost2, rtr_mac, res_time, verify_cmd )

for dtype in dscp_values:                       # loop through each dscp type running all tests for each
    dvalue = dscp_values[dtype]

    desc = "vm -> vm   dscp=%s" % dtype 
    run_one_test( tinfo, port1=None, port2=None, exip=None, vlan=None, oneway=False )

    desc = "vm:port -> vm   dscp=%s" % dtype 
    run_one_test( tinfo, port1=1982, port2=None, exip=None, vlan=None, oneway=False )

    desc = "vm -> vm:port   dscp=%s" % dtype 
    run_one_test( tinfo, port2=1982, port1=None, exip=None, vlan=None, oneway=False )

    desc = "vm -> external   dscp=%s" % dtype 
    run_one_test( tinfo, port1=None, port2=None, exip=exip, vlan=None, oneway=False )

#end

exit( 0 )

