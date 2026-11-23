import fcntl,os,sys,time
p=sys.argv[1]; sentinel=sys.argv[2]
f=open(p,'a+')
fcntl.flock(f,fcntl.LOCK_EX)
open(sentinel+'.held','w').write(str(os.getpid()))
while os.path.exists(sentinel): time.sleep(0.2)
os.remove(sentinel+'.held')
