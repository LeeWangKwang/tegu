#!/usr/bin/env python
# vi: ts=4 sw=4:

'''	Mnemonic:	tegu_ha.py
	Abstract:	High availability script for tegu. Pings other tegu's in the
                standby_list. If no other tegu is running, then makes the
                current tegu active. If it finds multiple tegu running, then
                determines whether current tegu should be shut down or not based
                on db timestamps. Assumes that db checkpoints from other tegus
                do not override our checkpoints

  	Date:		15 December 2015
	Author:		Kaustubh Joshi
	Mod:		2014 15 Dec - Created script
				2015 30 Jan - Minor fixes.
 ------------------------------------------------------------------------------

  Algorithm
 -----------
 The tegu_ha script runs on each tegu node in a continuous loop checking for
 hearbeats from all the tegu nodes found in the /etc/tegu/standby_list once
 every 5 secs (default). A heartbeat is obtained by invoking the "tegu ping"
 command via the HTTP API. If exactly one tegu is running, the script does
 nothing. If no tegu is running, then the local tegu is activated after
 waiting for 5*priority seconds to let a higher priority tegu take over
 first. A tegu node's priority is determined by its order in the standby_list
 file. If the current node's tegu is running and another tegu is found, then a
 conflict resolution process is invoked whereby the last modified timestamps
 from checkpoint files from both tegu's are compared, and the tegu with the
 older checkpoint is deactivated. The rationale is that the F5 only keeps a
 single tegu active at a time, so the tegu with the most recent checkpoint
 ought to be the active one.'''

import sys
import time
import os
import socket
import subprocess
import string

# Directory locations
TEGU_ROOT = os.getenv('TEGU_ROOT', '/var')              # Tegu root dir
LIBDIR = os.getenv('TEGU_LIBD', TEGU_ROOT+'/lib/tegu')
LOGDIR = os.getenv('TEGU_LOGD', TEGU_ROOT+'/log/tegu')
ETCDIR = os.getenv('TEGU_ETCD', '/etc/tegu')
CKPTDIR = os.getenv('TEGU_CKPTD', LIBDIR+'/chkpt')      # Checkpoints directory
LOGFILE = LOGDIR+'/tegu_ha.log'				            # log file for this script

# Tegu config parameters
TEGU_PORT = os.getenv('TEGU_PORT', 29444)		     # tegu's api listen port
TEGU_USER = os.getenv('TEGU_USER', 'tegu')           # run only under tegu user
TEGU_PROTO = 'http'

SSH_CMD = 'ssh -o StrictHostKeyChecking=no %s@%s '

DEACTIVATE_CMD = '/usr/bin/tegu_standby on;' \
    'killall tegu; killall tegu_agent'               # Command to kill tegu
ACTIVATE_CMD = '/usr/bin/tegu_standby off;' \
    '/usr/bin/start_tegu; /usr/bin/start_tegu_agent' # Command to start tegu

# Command to force checkpoint synchronization
SYNC_CMD = '/usr/bin/tegu_synch'

# HA Configuration
HEARTBEAT_SEC = 5                    # Heartbeat interval in seconds
PRI_WAIT_SEC = 5                     # Backoff to let higher prio tegu take over
STDBY_LIST = ETCDIR + '/standby_list'# list of other hosts that might run tegu

# if present then this is a standby machine and we don't start
STDBY_FILE = ETCDIR + '/standby'
VERBOSE = 0

def logit( msg ):
    t = time.gmtime()
    sys.stderr.write( "%4d/%02d/%02d %02d:%02d %s\n" % (t.tm_year, t.tm_mon, t.tm_mday, t.tm_hour, t.tm_min, msg ) )
    sys.stderr.flush()

def crit(msg):
    '''Print critical message to log'''
    logit( "CRI: " + msg )

def err(msg):
    '''Print error message to log'''
    logit( "ERR: " + msg )

def warn(msg):
    '''Print warning message to log'''
    #sys.stderr.write(msg)
    logit( "WRN: " + msg )

def ssh_cmd(host):
    '''Return ssh command line for host. Empty host imples local execution.'''
    if host != '':
        return SSH_CMD % (TEGU_USER, host)
    return ''

def get_checkpoint(host=''):
    '''Fetches checkpoint files from specified host.'''
    ssh_prefix = ssh_cmd(host)

    try:
        subprocess.check_call(ssh_prefix + SYNC_CMD, shell="True")
        return True
    except subprocess.CalledProcessError:
        warn("Could not sync chkpts from %s" % host)
    return False

def extract_dt( str, c1, c2 ):
    '''
        given a string assumed to be from a long ls listing or
        from a verbose tar output, extract the date and time
        and return the unix timestamp. If the time/date components
        are not recognisable, then returns 0.
    '''
    try:                             # prevent stack dump if missing time, or wrong format
        toks = string.split( str )
        ttoks = string.split( toks[c2], "." )     # ls listing has decimal which pythong %S cannot handle
        to = time.strptime( toks[c1] + " " + ttoks[0], "%Y-%m-%d %H:%M:%S" )    # build time object
        return int( time.mktime( to ) )
    except Exception:
        logit( "unable to build a timestamp from: %s and %s" % (toks[c1], ttoks[0]) )
        return 0

    return 0

def should_be_active(host):
    '''Returns True if host should be active as opposed to current node'''

    htoks = string.split( host, "." )				# need short name for ls command

    # Cmd to suss out the most recent checkpoint file that was in the most recent tar from host.
    # Sort to ensure order by date from tar output then take first. Redirect stderr to null because
    # python doesn't ignore closed pipe signals and thus they propigate causing sort and grep to complain
    # once for each line of output it tries to write after head closes the pipe.
    ts_r_cmd = 'tar -t -v --full-time -f $( ls -t ' + LIBDIR + '/chkpt_synch.' + htoks[0] + '.*.tgz | head -1 ) '\
        + '| (grep resmgr_ | sort -r -k 4,5 | head -1 ) 2>/dev/null'

    # cmd to suss out the long listing of the most recent of our checkpoint files. Specifically look for
    # resmgr_ files as there might be other files in the directory. -F and grep -v might be overkill, but
    # aren't harmful.
    ts_l_cmd = 'ls --full-time -tF ' + CKPTDIR + '/resmgr_* | grep -v "\\/\\$" 2>/dev/null' + '| head -1'

    # Pull latest checkpoint from remote node
    # If there's an error, the other guy shouldn't be primary
    if not get_checkpoint(host):
        return False
    if not get_checkpoint():
        return True

    # Check if we or they have latest checkpoint
    try:
        # Get clock skew
        logit( "checking clock skew: " + ssh_cmd(host) + 'date +%s' )
        time_r = int( subprocess.check_output(ssh_cmd(host) + '/bin/date +%s', shell=True ) )		 # I don't grok why shell=True is needed, but it fails without
        time_l = int( subprocess.check_output(ssh_cmd('') + 'date +%s', shell=True ) )
        skew = time_l-time_r

        # Get ours and their checkpoint file info
        ts_r_s = subprocess.check_output(ts_r_cmd, shell=True ) # get ls listing info string
        ts_l_s = subprocess.check_output(ts_l_cmd, shell=True )

        if ts_r_s == ""  or  ts_l_s == "":
            logit( "unable to find chkpt file info host:" + host )
            return false

        ts_r = extract_dt( ts_r_s, 3, 4 )           # convert listing strings in numeric timestamps
        ts_l = extract_dt( ts_l_s, 5, 6 )

        return ts_r+skew > ts_l or \
            (ts_r+skew == ts_l and host < socket.getfqdn())

    except subprocess.CalledProcessError:
        warn("Could not get chkpt timestamps")

    return False

def is_active(host='localhost'):
    '''Return True if tegu is running on host
       If host is None, check if tegu is running on current host
       Use ping API check, standby file may be inconsistent'''

    # must use no-proxy to avoid bleeding proxy servers installed on these machins which gum up the works
    # in addition, grep stderr redirected to avoid pipe complaints
    curl_str = ('curl --noproxy \'*\' --connect-timeout 3 -s -d "ping" %s://%s:%d/tegu/api ' + \
        '| grep -q -i pong 2>/dev/null') % (TEGU_PROTO, host, TEGU_PORT)			
    try:
        subprocess.check_call(curl_str, shell=True)
        return True
    except subprocess.CalledProcessError:
        return False

def deactivate_tegu(host=''):
    ''' Deactivate tegu on a given host. If host is omitted, local
        tegu is stopped. Returns True if successful, False on error.'''
    ssh_prefix = ssh_cmd(host)
    try:
        subprocess.check_call(ssh_prefix + DEACTIVATE_CMD, shell=True)
        return True
    except subprocess.CalledProcessError:
        return False

def activate_tegu(host=''):
    ''' Activate tegu on a given host. If host is omitted, local
        tegu is started. Returns True if successful, False on error.'''
    if host != '':
        host = SSH_CMD % (TEGU_USER, host)
    try:
        subprocess.check_call(host + ACTIVATE_CMD, shell=True)
        return True
    except subprocess.CalledProcessError:
        return False

def main_loop(standby_list, this_node, priority):
    '''Main heartbeat and liveness check loop'''
    priority_wait = False
    while True:
        if not priority_wait:
            # Normal heartbeat
            time.sleep(HEARTBEAT_SEC)
        else:
            # No tegu running. Wait for higher priority tegu to activate.
            time.sleep(PRI_WAIT_SEC*priority)

        i_am_active = is_active()
        any_active = i_am_active

        # Check for active tegus
        for host in standby_list:
            if host == this_node:
                continue

            host_active = is_active(host)

            # Check for split brain: 2 tegus active
            if i_am_active and host_active:
                logit( "checking for split" )
                host_active = should_be_active(host)
                if host_active:
                    logit( "deactivate myself, " + host + " already running" )
                    deactivate_tegu()      # Deactivate myself
                    i_am_active = False
                else:
                    logit( "deactivate " + host + " already running here" )
                    deactivate_tegu(host)  # Deactivate other tegu

            # Track that at-least one tegu is active
            any_active = any_active or host_active

        # If no active tegu, then we must try
        if not any_active:
            if priority_wait or priority == 0:
                logit( "no running tegu found, starting here" )
                priority_wait = False
                activate_tegu()            # Start local tegu
            else:
                priority_wait = True
    # end loop

def main():
    '''Main function'''

    logit( "tegu_ha v1.0 started" )

    # Ready list of standby tegu nodes and find us
    standby_list = [l.strip() for l in open(STDBY_LIST, 'r')]
    this_node = socket.getfqdn()
    try:
        priority = standby_list.index(this_node)
        standby_list.remove(this_node)
    except ValueError:
        #sys.stderr.write("Could not find host "+this_node+" in standby list")
        crit("Could not find host "+this_node+" in standby list: %s" % STDBY_LIST )
        sys.exit(1)

    # Loop forever listening to heartbeats
    main_loop(standby_list, this_node, priority)


if __name__ == '__main__'  or  __name__ == "main":
    main()
