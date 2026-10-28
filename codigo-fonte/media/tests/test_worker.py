import unittest,sys,tempfile
from unittest.mock import patch
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import worker
class WorkerTests(unittest.TestCase):
    def test_source_resolution_preserved(self):
        self.assertEqual(worker.dimensions({'aspect':'source','resolution':'source'},{'width':3840,'height':2160}),(3840,2160))
        self.assertEqual(worker.dimensions({'aspect':'source','resolution':'source'},{'width':640,'height':360}),(640,360))
    def test_source_resolution_changed_aspect(self):
        self.assertEqual(worker.dimensions({'aspect':'9:16','resolution':'source'},{'width':1920,'height':1080}),(1080,1920))
    def test_source_resolution_requires_source(self):
        with self.assertRaises(worker.MediaError): worker.dimensions({'aspect':'9:16','resolution':'source'})
    def test_source_resolution_resource_limit(self):
        with self.assertRaises(worker.MediaError): worker.dimensions({'aspect':'source','resolution':'source'},{'width':7680,'height':4320})
    def test_duration_choice_requires_concrete_value(self):
        meta={'duration':6,'fps':30,'has_video':True}
        with tempfile.TemporaryDirectory() as d:
            for choice in ('suggest','custom','propose'):
                with patch.object(worker,'probe',return_value=meta):
                    with self.assertRaisesRegex(worker.MediaError,'Defina a duração'):
                        worker.edit({'output_dir':d,'project':{'duration':choice}})
    def test_timeline_remaps(self):
        g=worker.timeline([{'start':1,'end':3},{'start':5,'end':7}],8,30)
        self.assertEqual(g['duration'],4)
        words=[{'start':5.2,'end':5.8,'word':'Olá'}]
        result=worker.remap_transcript([{'start':5.2,'end':5.8,'text':'Olá','words':words}],g)
        self.assertAlmostEqual(result[0]['start'],2.2)
        self.assertEqual(result[0]['text'],'Olá')
    def test_overlap_rejected(self):
        with self.assertRaises(worker.MediaError):worker.timeline([{'start':0,'end':4},{'start':3,'end':6}],8)
    def test_nonfinite_rejected(self):
        with self.assertRaises(worker.MediaError):worker.number(float('nan'),0,0,10)
    def test_silence_handles(self):
        r=worker.silence_ranges('silence_start: 2\nsilence_end: 4',6,.2)
        self.assertEqual(r,[{'start':0,'end':2.2},{'start':3.8,'end':6}])
    def test_format_individual(self):
        a={'aspect':'9:16','brand':{'aspect':'16:9','resolution':4000}}
        self.assertEqual(worker.dimensions(a),(1080,1920))
        self.assertEqual(worker.dimensions({'aspect':'1:1'}),(1080,1080))
    def test_brand_does_not_determine_duration(self):
        self.assertEqual(worker.brand_of({'project':{'duration':20,'brand_snapshot':{'name':'A'}}}),{'name':'A'})
    def test_scope_escape(self):
        with tempfile.TemporaryDirectory() as d:
            base=Path(d);(base/'allowed').mkdir();(base/'secret.txt').write_text('x');(base/'allowed'/'link').symlink_to(base/'secret.txt')
            with self.assertRaises(worker.MediaError):worker.source_path({'allowed_roots':[str(base/'allowed')]},base/'allowed'/'link')
    def test_color(self):
        self.assertEqual(worker.color({'colors':[{'role':'accent','hex':'#42ABCD'}]},'accent','#000000'),'#42ABCD')
        self.assertEqual(worker.color({'colors':[{'role':'accent','hex':'red;evil'}]},'accent','#000000'),'#000000')
    def test_timestamp(self):
        self.assertEqual(worker.timestamp(61.234),'00:01:01,234')
        self.assertEqual(worker.timestamp(61.234,vtt=True),'00:01:01.234')
        self.assertEqual(worker.timestamp(61.23,ass=True),'0:01:01.23')
    def test_no_company_audio(self):
        self.assertEqual(worker.audio_config({'brand':{'audio':{'mode':'none'}}})['mode'],'original')
    def test_empty_ranges(self):
        g=worker.timeline(None,5,30);self.assertEqual(len(g['segments']),1)
    def test_words_cross_cut(self):
        g=worker.timeline([{'start':0,'end':1},{'start':2,'end':3}],3)
        s=[{'start':.5,'end':2.5,'text':'um dois três','words':[{'start':.5,'end':.8,'word':'um'},{'start':1.3,'end':1.6,'word':'dois'},{'start':2.1,'end':2.5,'word':'três'}]}]
        r=worker.remap_transcript(s,g);self.assertEqual([x['text'] for x in r],['um','três'])
    def test_cancel(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'cancel';p.touch()
            with self.assertRaises(worker.MediaError):worker.check_cancel({'cancel_file':str(p)})
if __name__=='__main__':unittest.main(verbosity=2)
