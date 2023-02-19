package highway

import (
	"crypto/md5"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/Mrs4s/MiraiGo/binary"
	"github.com/Mrs4s/MiraiGo/client/pb"
	"github.com/Mrs4s/MiraiGo/internal/proto"
)

type Transaction struct {
	CommandID int32
	Body      io.Reader
	Sum       []byte // md5 sum of body
	Size      int64  // body size
	Ticket    []byte
	Ext       []byte
	Encrypt   bool
}

func (bdh *Transaction) encrypt(key []byte) error {
	if !bdh.Encrypt {
		return nil
	}
	if len(key) == 0 {
		return errors.New("session key not found. maybe miss some packet?")
	}
	bdh.Ext = binary.NewTeaCipher(key).Encrypt(bdh.Ext)
	return nil
}

func (s *Session) retry(upload func(s *Session, addr Addr, trans *Transaction) ([]byte, error), trans *Transaction) ([]byte, error) {
	// try to find a available server
	for _, addr := range s.SsoAddr {
		r, err := upload(s, addr, trans)
		if err == nil {
			return r, nil
		}
		if _, ok := err.(net.Error); ok {
			// try another server
			// TODO: delete broken servers?
			continue
		}
		return nil, err
	}
	return nil, errors.New("cannot found available server")
}

func (s *Session) UploadBDH(trans Transaction) ([]byte, error) {
	// encrypt ext data
	if err := trans.encrypt(s.SessionKey); err != nil {
		return nil, err
	}

	pc, err := s.selectConn()
	if err != nil {
		return nil, err
	}
	defer s.putIdleConn(pc)

	reader := binary.NewNetworkReader(pc.conn)
	const chunkSize = 256 * 1024
	var rspExt []byte
	offset := 0
	chunk := make([]byte, chunkSize)
	for {
		chunk = chunk[:cap(chunk)]
		rl, err := io.ReadFull(trans.Body, chunk)
		if rl == 0 {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			chunk = chunk[:rl]
		}
		ch := md5.Sum(chunk)
		head, _ := proto.Marshal(&pb.ReqDataHighwayHead{
			MsgBasehead: &pb.DataHighwayHead{
				Version:   1,
				Uin:       s.Uin,
				Command:   _REQ_CMD_DATA,
				Seq:       s.nextSeq(),
				Appid:     s.AppID,
				Dataflag:  4096,
				CommandId: trans.CommandID,
				LocaleId:  2052,
			},
			MsgSeghead: &pb.SegHead{
				Filesize:      trans.Size,
				Dataoffset:    int64(offset),
				Datalength:    int32(rl),
				Serviceticket: trans.Ticket,
				Md5:           ch[:],
				FileMd5:       trans.Sum,
			},
			ReqExtendinfo: trans.Ext,
		})
		offset += rl
		buffers := frame(head, chunk)
		_, err = buffers.WriteTo(pc.conn)
		if err != nil {
			return nil, errors.Wrap(err, "write pc error")
		}
		rspHead, err := readResponse(reader)
		if err != nil {
			return nil, errors.Wrap(err, "highway upload error")
		}
		if rspHead.ErrorCode != 0 {
			return nil, errors.Errorf("upload failed: %d", rspHead.ErrorCode)
		}
		if rspHead.RspExtendinfo != nil {
			rspExt = rspHead.RspExtendinfo
		}
		if rspHead.MsgSeghead != nil && rspHead.MsgSeghead.Serviceticket != nil {
			trans.Ticket = rspHead.MsgSeghead.Serviceticket
		}
	}
	return rspExt, nil
}

func (s *Session) UploadBDHMultiThread(trans Transaction) ([]byte, error) {
	// for small file and small thread count,
	// use UploadBDH instead of UploadBDHMultiThread
	if trans.Size < 1024*1024*3 {
		return s.UploadBDH(trans)
	}

	// encrypt ext data
	if err := trans.encrypt(s.SessionKey); err != nil {
		return nil, err
	}

	// pick a address
	// TODO: pick smarter
	pc, err := s.selectConn()
	if err != nil {
		return nil, err
	}
	addr := pc.addr
	s.putIdleConn(pc)

	// TODO: use idle conn
	const blockSize int64 = 256 * 1024
	const threadCount = 4
	var (
		rspExt          []byte
		completedThread uint32
		cond            = sync.NewCond(&sync.Mutex{})
		offset          = int64(0)
		count           = (trans.Size + blockSize - 1) / blockSize
		id              = 0
	)
	doUpload := func() error {
		// send signal complete uploading
		defer func() {
			atomic.AddUint32(&completedThread, 1)
			cond.Signal()
		}()

		pc, err := s.connect(addr)
		if err != nil {
			return err
		}
		// defer s.putIdleConn(pc) // TODO: should we put back?

		reader := binary.NewNetworkReader(pc.conn)
		chunk := make([]byte, blockSize)
		for {
			cond.L.Lock() // lock protect reading
			off := offset
			offset += blockSize
			id++
			if int64(id) == count { // last
				for atomic.LoadUint32(&completedThread) != uint32(threadCount-1) {
					cond.Wait()
				}
			} else if int64(id) > count {
				cond.L.Unlock()
				break
			}
			chunk = chunk[:blockSize]
			n, err := io.ReadFull(trans.Body, chunk)
			cond.L.Unlock()

			if n == 0 {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				chunk = chunk[:n]
			}
			ch := md5.Sum(chunk)
			head, _ := proto.Marshal(&pb.ReqDataHighwayHead{
				MsgBasehead: &pb.DataHighwayHead{
					Version:   1,
					Uin:       s.Uin,
					Command:   _REQ_CMD_DATA,
					Seq:       s.nextSeq(),
					Appid:     s.AppID,
					Dataflag:  4096,
					CommandId: trans.CommandID,
					LocaleId:  2052,
				},
				MsgSeghead: &pb.SegHead{
					Filesize:      trans.Size,
					Dataoffset:    off,
					Datalength:    int32(n),
					Serviceticket: trans.Ticket,
					Md5:           ch[:],
					FileMd5:       trans.Sum,
				},
				ReqExtendinfo: trans.Ext,
			})
			buffers := frame(head, chunk)
			_, err = buffers.WriteTo(pc.conn)
			if err != nil {
				return errors.Wrap(err, "write conn error")
			}
			rspHead, err := readResponse(reader)
			if err != nil {
				return errors.Wrap(err, "highway upload error")
			}
			if rspHead.ErrorCode != 0 {
				return errors.Errorf("upload failed: %d", rspHead.ErrorCode)
			}
			if rspHead.RspExtendinfo != nil {
				rspExt = rspHead.RspExtendinfo
			}
		}
		return nil
	}

	group := errgroup.Group{}
	for i := 0; i < threadCount; i++ {
		group.Go(doUpload)
	}
	return rspExt, group.Wait()
}
