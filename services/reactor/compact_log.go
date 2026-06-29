package reactor

import "devicecode-go/types"

type compactStateFlag struct {
	bit   types.ChargerStateBits
	label string
}

type compactStatusFlag struct {
	bit   types.ChargeStatusBits
	label string
}

type compactSystemFlag struct {
	bit   types.SystemStatus
	label string
}

var compactChargerStateFlags = [...]compactStateFlag{
	{types.EqualizeCharge, "eq"},
	{types.AbsorbCharge, "abs"},
	{types.ChargerSuspended, "susp"},
	{types.Precharge, "pre"},
	{types.CCCVCharge, "cccv"},
	{types.NTCPause, "ntc"},
	{types.TimerTerm, "timer"},
	{types.COverXTerm, "c/x"},
	{types.MaxChargeTimeFault, "tmax"},
	{types.BatMissingFault, "bat?"},
	{types.BatShortFault, "short"},
}

var compactChargeStatusFlags = [...]compactStatusFlag{
	{types.VinUvclActive, "uvcl"},
	{types.IinLimitActive, "ilim"},
	{types.ConstCurrent, "cc"},
	{types.ConstVoltage, "cv"},
}

var compactSystemStatusFlags = [...]compactSystemFlag{
	{types.ChargerEnabled, "en"},
	{types.MpptEnPin, "mppt"},
	{types.EqualizeReq, "eqreq"},
	{types.DrvccGood, "drvcc"},
	{types.CellCountError, "cell!"},
	{types.OkToCharge, "ok"},
	{types.NoRt, "noRT"},
	{types.ThermalShutdown, "hot"},
	{types.VinOvlo, "ovlo"},
	{types.VinGtVbat, "vin>bat"},
	{types.IntvccGt4p3V, "4v3"},
	{types.IntvccGt2p8V, "2v8"},
}

func logHex16(v uint16) {
	const hexd = "0123456789ABCDEF"
	var b [4]byte
	b[0] = hexd[(v>>12)&0xF]
	b[1] = hexd[(v>>8)&0xF]
	b[2] = hexd[(v>>4)&0xF]
	b[3] = hexd[v&0xF]
	log.Print(b[:])
}

func logCompactChargerState(bits uint16) {
	log.Print("st=0x")
	logHex16(bits)
	log.Print("{")
	first := true
	for _, f := range compactChargerStateFlags {
		if bits&uint16(f.bit) == 0 {
			continue
		}
		if !first {
			log.Print(",")
		} else {
			first = false
		}
		log.Print(f.label)
	}
	log.Print("}")
}

func logCompactChargeStatus(bits uint16) {
	log.Print("ss=0x")
	logHex16(bits)
	log.Print("{")
	first := true
	for _, f := range compactChargeStatusFlags {
		if bits&uint16(f.bit) == 0 {
			continue
		}
		if !first {
			log.Print(",")
		} else {
			first = false
		}
		log.Print(f.label)
	}
	log.Print("}")
}

func logCompactSystemStatus(bits uint16) {
	log.Print("sys=0x")
	logHex16(bits)
	log.Print("{")
	first := true
	for _, f := range compactSystemStatusFlags {
		if bits&uint16(f.bit) == 0 {
			continue
		}
		if !first {
			log.Print(",")
		} else {
			first = false
		}
		log.Print(f.label)
	}
	log.Print("}")
}

func logCompactChargerBits(stateBits, statusBits, systemBits uint16) {
	log.Print(" ")
	logCompactChargerState(stateBits)
	log.Print(" ")
	logCompactChargeStatus(statusBits)
	log.Print(" ")
	logCompactSystemStatus(systemBits)
}
