#!/usr/bin/python3
import collections, errno, json, os, pathlib, subprocess, threading, time

import argparse
parser=argparse.ArgumentParser(description='macOS NFS restart measurement; creates only private state, never the configured daemon')
parser.add_argument('--binary',required=True)
parser.add_argument('--output',required=True)
parser.add_argument('--pause-seconds',type=int,default=0)
parser.add_argument('--runs',type=int,default=5)
options=parser.parse_args()
ROOT=pathlib.Path(options.output).resolve()
ROOT.mkdir(parents=True,exist_ok=True)
RUN=ROOT/('r'+format(time.time_ns(),'x'))
RUN.mkdir()
STATE,MOUNT,SOURCE=[RUN/x for x in ('state','mount','source')]
SOURCE.mkdir()
BIN=str(pathlib.Path(options.binary).resolve())
ENV = dict(os.environ, OVNFS_STATE=str(STATE), OVNFS_MOUNT=str(MOUNT))
LOG = open(RUN / 'samples.jsonl', 'a', buffering=1)
mutex = threading.Lock()
def emit(**kw):
    with mutex: LOG.write(json.dumps(dict(t=time.monotonic(), wall=time.time(), **kw)) + '\n')
def ov(*args, timeout=40):
    return subprocess.run([BIN, '--state', str(STATE), '--mount', str(MOUNT), *args], env=ENV, text=True, capture_output=True, timeout=timeout)
def git(*args, cwd=SOURCE):
    return subprocess.run(['git', *args], cwd=cwd, text=True, capture_output=True, timeout=120)
for args in [('init',), ('config','user.name','Restart Test'), ('config','user.email','test@localhost')]:
    assert git(*args).returncode == 0
(SOURCE / 'probe.txt').write_text('restart probe\n')
for i in range(5000 if options.pause_seconds else 200): (SOURCE / ('file-%04d' % i)).write_text('fixture\n')
assert git('add','.').returncode == 0
assert git('commit','-m','fixture').returncode == 0
assert git('branch','merge-target').returncode == 0
for name in ('probe','git-loop','spare'):
    r=ov('new', str(SOURCE), '--rev','HEAD','--name',name)
    emit(kind='setup', name=name, rc=r.returncode, out=r.stdout, err=r.stderr)
    if r.returncode: raise SystemExit('setup failed: '+r.stderr)
print(str(RUN), flush=True)
def serving_marker(pid):
    try:
        identity=(STATE/'state.lock').read_bytes()[16:].strip()
        fields=identity.split()
        return bool(fields) and fields[0]==str(pid).encode() and (STATE/'state.ready').read_bytes().strip()==identity
    except OSError:
        return False
CHECKOUT=MOUNT / 'probe'
GITCO=MOUNT / 'git-loop'
round_no=0
stop=threading.Event()
def sample(kind, fn):
    while not stop.is_set():
        start=time.monotonic()
        try:
            value=fn(); emit(kind=kind, run=round_no, begin=start, duration=time.monotonic()-start, ok=True, value=value)
        except (OSError, subprocess.SubprocessError) as e:
            emit(kind=kind, run=round_no, begin=start, duration=time.monotonic()-start, ok=False, errno=getattr(e,'errno',None), error=str(e))
        stop.wait(max(0, .1-(time.monotonic()-start)))
def mounts():
    p=subprocess.run(['/sbin/mount'],text=True,capture_output=True,timeout=2)
    return next((x for x in p.stdout.splitlines() if ' on '+str(MOUNT)+' ' in x), '')
def pwd():
    p=subprocess.run(['/bin/pwd'],cwd=CHECKOUT,text=True,capture_output=True,timeout=120)
    if p.returncode: raise OSError(errno.EIO,p.stderr)
    return p.stdout.strip()
def gitloop():
    while not stop.is_set():
        for args in [('status','--porcelain'),('commit','--allow-empty','-m','restart probe'),('merge','--no-edit','--no-ff','merge-target')]:
            start=time.monotonic()
            try:
                if args[0]=='merge':
                    side=git('commit-tree','HEAD^{tree}','-p','HEAD','-m','side',cwd=GITCO)
                    if side.returncode: raise OSError(errno.EIO, side.stderr)
                    args=(*args[:-1],side.stdout.strip())
                p=git(*args,cwd=GITCO)
                emit(kind='git', run=round_no, command=args[0], begin=start, duration=time.monotonic()-start, ok=p.returncode==0, rc=p.returncode, err=p.stderr)
            except (OSError,subprocess.SubprocessError) as e:
                emit(kind='git', run=round_no, command=args[0], begin=start, duration=time.monotonic()-start, ok=False, errno=getattr(e,'errno',None), error=str(e))
        stop.wait(.1)
threads=[threading.Thread(target=sample,args=(k,f),daemon=True) for k,f in [('mount',mounts),('stat',lambda: CHECKOUT.stat().st_ino),('read',lambda: (CHECKOUT/'probe.txt').read_text()),('pwd',pwd)]]
threads.append(threading.Thread(target=gitloop,daemon=True))
for t in threads: t.start()
time.sleep(1)
for n in range(1,options.runs+1):
    round_no=n
    start=time.monotonic()
    p=subprocess.Popen([BIN,'--state',str(STATE),'--mount',str(MOUNT),'restart'],env=ENV,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    children=[]
    if options.pause_seconds and n==1:
        import fcntl, signal
        paused=None
        state_lock=open(STATE/'state.lock','r+')
        # Identify only a child spawned by this exact restart process, then verify its private state in argv.
        import ctypes
        proc=ctypes.CDLL("/usr/lib/libproc.dylib")
        children_buf=(ctypes.c_int*256)()
        for _ in range(20000):
            count=proc.proc_listchildpids(p.pid,children_buf,ctypes.sizeof(children_buf))
            found=[]
            for childpid in children_buf[:max(0,count)]:
                buf=ctypes.create_string_buffer(4096)
                if proc.proc_pidpath(childpid,buf,len(buf))>0 and buf.value.decode()==BIN:
                    found.append(childpid)
            if found:
                candidate=found[0]
                os.kill(candidate,signal.SIGSTOP)
                # If admin.sock already exists this was too late to simulate cold loading.
                if serving_marker(candidate):
                    os.kill(candidate,signal.SIGCONT)
                    emit(kind='pause-too-late',pid=candidate)
                    break
                for attempt in range(200):
                    try:
                        fcntl.flock(state_lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
                        fcntl.flock(state_lock,fcntl.LOCK_UN)
                    except BlockingIOError:
                        if serving_marker(candidate):
                            emit(kind='pause-too-late',pid=candidate)
                        else:
                            paused=candidate
                        break
                    os.kill(candidate,signal.SIGCONT)
                    time.sleep(.001)
                    os.kill(candidate,signal.SIGSTOP)
                if not paused: os.kill(candidate,signal.SIGCONT)
                break
            time.sleep(.0001)
        if paused:
            emit(kind='paused-verified-state-holder',pid=paused)
            def resume():
                time.sleep(options.pause_seconds)
                os.kill(paused,signal.SIGCONT)
                emit(kind='resumed',pid=paused)
            threading.Thread(target=resume,daemon=True).start()
            # The restart will release start.lock after its 15-second startup wait.
            time.sleep(16)
            for i in range(3):
                children.append(subprocess.Popen([BIN,'--state',str(STATE),'--mount',str(MOUNT),'new',str(SOURCE),'--rev','HEAD','--name',f'race-{i}'],env=ENV,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE))
        else:
            emit(kind='could-not-pause-startup')
    else:
        if n==options.runs:
            # Race the exact auto-start entry point as soon as the old admin socket closes.
            import socket
            for _ in range(2000):
                s=socket.socket(socket.AF_UNIX); s.settimeout(.01)
                try: s.connect(str(STATE/'admin.sock')); ready=True
                except OSError: ready=False
                finally: s.close()
                if not ready: break
                time.sleep(.001)
            for i in range(3):
                children.append(subprocess.Popen([BIN,'--state',str(STATE),'--mount',str(MOUNT),'new',str(SOURCE),'--rev','HEAD','--name',f'race-{i}'],env=ENV,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE))
    try:
        out,err=p.communicate(timeout=45)
        emit(kind='restart',run=n,begin=start,duration=time.monotonic()-start,rc=p.returncode,out=out,err=err)
    except subprocess.TimeoutExpired:
        emit(kind='restart-timeout',run=n,pid=p.pid)
        print('restart timeout; isolated evidence retained',flush=True)
        break
    for i,child in enumerate(children):
        try:
            out,err=child.communicate(timeout=30)
            emit(kind='parallel-new',run=n,index=i,rc=child.returncode,out=out,err=err)
            if child.returncode == 0:
                assert (pathlib.Path(out.strip())/'probe.txt').read_text() == 'restart probe\n'
        except subprocess.TimeoutExpired: emit(kind='parallel-new-timeout',run=n,index=i,pid=child.pid)
    print('restart',n,'rc',p.returncode,'seconds',round(time.monotonic()-start,3),flush=True)
    time.sleep(max(2,options.pause_seconds + 1 - (time.monotonic()-start)) if options.pause_seconds and n==1 else 2)
stop.set()
for t in threads: t.join(timeout=16)
# Only this harness's private daemon/mount is stopped. Never remove test checkouts or state.
r=ov('stop',timeout=30)
emit(kind='cleanup-stop',rc=r.returncode,out=r.stdout,err=r.stderr)
print('finished',str(RUN),flush=True)
