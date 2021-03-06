package kvpaxos

import "net"
import "fmt"
import "net/rpc"
import "log"
import "paxos"
import "sync"
import "sync/atomic"
import "os"
import "syscall"
import "encoding/gob"
import "math/rand"

import "time"

// for debugging
const Debug = 0 
func DPrintf(format string, a ...interface{}) {
	if Debug > 0 {
		fmt.Printf(format, a...)
	}
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	OpID  int64
	Op    string
	Key   string
	Value string
}

const TTLofFilter = 10
const TickInterval = 100 * time.Millisecond

type KVPaxos struct {
	mu         sync.Mutex
	l          net.Listener
	me         int
	dead       int32 // for testing
	unreliable int32 // for testing
	px         *paxos.Paxos

	// Your definitions here.
	seq        int
	
	filters    map[int64]int
	replies    map[int64]interface{}
	kvstore    map[string]string
}

func (kv *KVPaxos) filterDuplicate(opid int64) (interface{}, bool) {
	_, ok := kv.filters[opid] // kv.filters.Get(opid)
	if !ok { // no filter found 
		return nil, false
	}
	kv.filters[opid] = TTLofFilter
	rp, _ := kv.replies[opid] //kv.replies.Get(opid)
	return rp, true
}

func (kv *KVPaxos) recordOperation(opid int64, reply interface{}) {
	kv.filters[opid] = TTLofFilter
	kv.replies[opid] = reply
}

func (kv *KVPaxos) sync(xop *Op) {
	seq := kv.seq // kv.keyseqs[xop.Key]
	DPrintf("----- server %d sync %v\n", kv.me, xop)
	
	wait_init := func() time.Duration {
		return /* (1 + time.Duration(rand.Intn(4))) * */ 10 * time.Millisecond
	}
	
	wait := wait_init()
	for {
		fate, v := kv.px.Status(seq)
		if fate == paxos.Decided {
			op := v.(Op)
			DPrintf("----- server %d : seq %d : %v\n", kv.me, seq, op)
			if xop.OpID == op.OpID {
				break
			} else if op.Op == Put || op.Op == Append {
				kv.doPutAppend(op.Op, op.Key, op.Value)
				kv.recordOperation(op.OpID, &PutAppendReply{OK})
			} else {
				value, ok := kv.doGet(op.Key)
				if ok {
					kv.recordOperation(op.OpID, &GetReply{OK, value})
				} else {
					kv.recordOperation(op.OpID, &GetReply{ErrNoKey, ""})
				}
			}
			kv.px.Done(seq)
			seq++
			wait = wait_init()
		} else { // Pending
			if fate == paxos.Forgotten {
				DPrintf("--- bug :( ---" )
			}
			DPrintf("----- server %d starts a new paxos instance : %d %v\n", kv.me, seq, xop)
			kv.px.Start(seq, *xop)
			time.Sleep(wait)
			if wait < time.Second {
				wait *= 2
			}
		}
	}
	kv.px.Done(seq)
	kv.seq = seq + 1
}

func (kv *KVPaxos) doGet(key string) (value string, ok bool) {
	//DPrintf("Get : server %d : key %s\n", kv.me, key)
	value, ok = kv.kvstore[key]
	if ok {
		DPrintf("Get : server %d : key %s : value %s\n", kv.me, key, value)
	} else {
		DPrintf("Get : server %d : key %s : ErrNoKey\n", kv.me, key)
	}
	return 
}

func (kv *KVPaxos) Get(args *GetArgs, reply *GetReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	DPrintf("RPC Get : server %d : key %s\n", kv.me, args.Key)
	
	rp, yes := kv.filterDuplicate(args.OpID)
	if yes {
		DPrintf("dup-op detected : %v\n", args)
		reply.Value = rp.(*GetReply).Value
		reply.Err = rp.(*GetReply).Err
		return nil
	}
	
	xop := &Op{args.OpID, Get, args.Key, ""}
	kv.sync(xop)
	value, ok := kv.doGet(xop.Key)
	if ok {
		reply.Err = OK
		reply.Value = value
	} else {
		reply.Err = ErrNoKey
	}
	
	kv.recordOperation(args.OpID, reply)

	return nil
}

func (kv *KVPaxos) doPutAppend(op string, key string, value string) {
	DPrintf("%s : server %d : key %s : value %s\n", op, kv.me, key, value)
	if op == Put {
		kv.kvstore[key] = value	
	} else {
		kv.kvstore[key] += value
		DPrintf("--------------------------------> %s\n", kv.kvstore[key])
	}
}

func (kv *KVPaxos) PutAppend(args *PutAppendArgs, reply *PutAppendReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	DPrintf("RPC PutAppend : server %d : op %s : key %s : val %s\n", 
		kv.me, args.Op, args.Key, args.Value)
	
	rp, yes := kv.filterDuplicate(args.OpID) 
	if yes {
		DPrintf("dup-op detected : %v\n", args)
		reply.Err = rp.(*PutAppendReply).Err
		return nil
	}
	
	xop := &Op{args.OpID, args.Op, args.Key, args.Value}
	kv.sync(xop)
	kv.doPutAppend(xop.Op, xop.Key, xop.Value)
	reply.Err = OK
	
	kv.recordOperation(args.OpID, reply)
	
	return nil
}

func (kv *KVPaxos) cleanFilters() {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	for id := range kv.filters {
		kv.filters[id]--
		if kv.filters[id] <= 0 {
			delete(kv.filters, id)
			delete(kv.replies, id)
		}
	}
}

// tell the server to shut itself down.
// please do not change these two functions.
func (kv *KVPaxos) kill() {
	DPrintf("Kill(%d): die\n", kv.me)
	atomic.StoreInt32(&kv.dead, 1)
	kv.l.Close()
	kv.px.Kill()
}

// call this to find out if the server is dead.
func (kv *KVPaxos) isdead() bool {
	return atomic.LoadInt32(&kv.dead) != 0
}

// please do not change these two functions.
func (kv *KVPaxos) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&kv.unreliable, 1)
	} else {
		atomic.StoreInt32(&kv.unreliable, 0)
	}
}

func (kv *KVPaxos) isunreliable() bool {
	return atomic.LoadInt32(&kv.unreliable) != 0
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
//
func StartServer(servers []string, me int) *KVPaxos {
	// call gob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	gob.Register(Op{})

	kv := new(KVPaxos)
	kv.me = me

	// Your initialization code here.
	kv.filters = make(map[int64]int)
	kv.replies = make(map[int64]interface{})
	kv.kvstore = make(map[string]string)

	rpcs := rpc.NewServer()
	rpcs.Register(kv)

	kv.px = paxos.Make(servers, me, rpcs)

	os.Remove(servers[me])
	l, e := net.Listen("unix", servers[me])
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	kv.l = l


	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for kv.isdead() == false {
			conn, err := kv.l.Accept()
			if err == nil && kv.isdead() == false {
				if kv.isunreliable() && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if kv.isunreliable() && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					c1 := conn.(*net.UnixConn)
					f, _ := c1.File()
					err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
					if err != nil {
						fmt.Printf("shutdown: %v\n", err)
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && kv.isdead() == false {
				fmt.Printf("KVPaxos(%v) accept: %v\n", me, err.Error())
				kv.kill()
			}
		}
	}()

	go func() {
		for kv.isdead() == false {
			time.Sleep(TickInterval)
			kv.cleanFilters()
		}
	}()

	return kv
}
